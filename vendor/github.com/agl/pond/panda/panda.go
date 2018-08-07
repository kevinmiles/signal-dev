package panda

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/agl/pond/panda/rijndael"
	"github.com/golang/protobuf/proto"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"

	panda_proto "github.com/agl/pond/panda/proto"
)

const (
	generatedSecretStringPrefix = "r!"
	// generatedSecretStringPrefix2 is used to indicate that scrypt should
	// be skipped because the generated secret is sufficiently random.
	// These strings are not output, yet, but they are handled. In the
	// future, the code will start outputting them - although this will
	// (silently, painfully) break clients that are too old to support
	// them.
	generatedSecretStringPrefix2 = "r["
)

// NewSecretString generates a random, human readable string with a special
// form that includes a checksum which allows typos to be rejected (so long as
// the typo isn't in the first two letters).
func NewSecretString(rand io.Reader) string {
	b := make([]byte, 16, 18)
	if _, err := rand.Read(b); err != nil {
		panic("error reading from rand: " + err.Error())
	}

	// The use of SHA-256 is overkill, but we already use SHA-256 so it's
	// more parsimonious than importing a more suitable hash function.
	h := sha256.New()
	h.Write(b)
	digest := h.Sum(nil)

	b = append(b, digest[0], digest[1])
	return generatedSecretStringPrefix + hex.EncodeToString(b)
}

// isValidSecretString returns true if s is of the form generated by
// NewSecretString.
func isValidSecretString(s string) bool {
	if !strings.HasPrefix(s, generatedSecretStringPrefix) &&
		!strings.HasPrefix(s, generatedSecretStringPrefix2) {
		return false
	}
	s = s[len(generatedSecretStringPrefix):]

	var b [18]byte
	if len(s)%2 == 1 || hex.DecodedLen(len(s)) != len(b) {
		return false
	}
	if _, err := hex.Decode(b[:], []byte(s)); err != nil {
		return false
	}

	h := sha256.New()
	h.Write(b[:16])
	digest := h.Sum(nil)

	return b[16] == digest[0] && b[17] == digest[1]
}

// IsAcceptableSecretString returns true if s should be accepted as a secret
// string. The only strings that will be rejected are those that start with
// generatedSecretStringPrefix but don't have a matching checksum.
func IsAcceptableSecretString(s string) bool {
	if !strings.HasPrefix(s, generatedSecretStringPrefix) &&
		!strings.HasPrefix(s, generatedSecretStringPrefix2) {
		return true
	}

	return isValidSecretString(s)
}

var ShutdownErr = errors.New("panda: shutdown requested")

type SharedSecret struct {
	Secret           string
	Cards            CardStack
	Day, Month, Year int
	Hours, Minutes   int
}

func (s *SharedSecret) isStrongRandom() bool {
	return strings.HasPrefix(s.Secret, generatedSecretStringPrefix2) && isValidSecretString(s.Secret)
}

func (s *SharedSecret) toProto() *panda_proto.KeyExchange_SharedSecret {
	ret := new(panda_proto.KeyExchange_SharedSecret)
	if len(s.Secret) > 0 {
		ret.Secret = proto.String(s.Secret)
	}
	if s.Cards.NumDecks > 0 {
		ret.NumDecks = proto.Int32(int32(s.Cards.NumDecks))
		canonical := s.Cards.Canonicalise()
		ret.CardCount = canonical.counts[:]
	}
	if s.Year != 0 {
		ret.Time = &panda_proto.KeyExchange_SharedSecret_Time{
			Day:     proto.Int32(int32(s.Day)),
			Month:   proto.Int32(int32(s.Month)),
			Year:    proto.Int32(int32(s.Year)),
			Hours:   proto.Int32(int32(s.Hours)),
			Minutes: proto.Int32(int32(s.Minutes)),
		}
	}

	return ret
}

func newSharedSecret(p *panda_proto.KeyExchange_SharedSecret) (*SharedSecret, bool) {
	ret := &SharedSecret{
		Secret:  p.GetSecret(),
		Day:     int(p.Time.GetDay()),
		Month:   int(p.Time.GetMonth()),
		Year:    int(p.Time.GetYear()),
		Hours:   int(p.Time.GetHours()),
		Minutes: int(p.Time.GetMinutes()),
	}
	ret.Cards.NumDecks = int(p.GetNumDecks())
	if ret.Cards.NumDecks > 0 {
		if len(p.CardCount) != numCards {
			return nil, false
		}
		copy(ret.Cards.counts[:], p.CardCount)
	} else {
		if len(ret.Secret) == 0 {
			return nil, false
		}
	}

	return ret, true
}

type MeetingPlace interface {
	Padding() int
	Exchange(log func(string, ...interface{}), id, message []byte, shutdown chan struct{}) ([]byte, error)
}

type KeyExchange struct {
	sync.Mutex

	Log          func(string, ...interface{})
	Testing      bool
	ShutdownChan chan struct{}

	rand         io.Reader
	status       panda_proto.KeyExchange_Status
	meetingPlace MeetingPlace
	sharedSecret *SharedSecret
	serialised   []byte
	kxBytes      []byte

	key, meeting1, meeting2 [32]byte
	dhPublic, dhPrivate     [32]byte
	sharedKey               [32]byte
	message1, message2      []byte
}

func NewKeyExchange(rand io.Reader, meetingPlace MeetingPlace, sharedSecret *SharedSecret, kxBytes []byte) (*KeyExchange, error) {
	if 24 /* nonce */ +4 /* length */ +len(kxBytes)+secretbox.Overhead > meetingPlace.Padding() {
		return nil, errors.New("panda: key exchange too large for meeting place")
	}

	kx := &KeyExchange{
		Log:          func(format string, args ...interface{}) {},
		rand:         rand,
		meetingPlace: meetingPlace,
		status:       panda_proto.KeyExchange_INIT,
		sharedSecret: sharedSecret,
		kxBytes:      kxBytes,
	}

	if _, err := io.ReadFull(kx.rand, kx.dhPrivate[:]); err != nil {
		return nil, err
	}
	curve25519.ScalarBaseMult(&kx.dhPublic, &kx.dhPrivate)
	kx.updateSerialised()

	return kx, nil
}

func UnmarshalKeyExchange(rand io.Reader, meetingPlace MeetingPlace, serialised []byte) (*KeyExchange, error) {
	var p panda_proto.KeyExchange
	if err := proto.Unmarshal(serialised, &p); err != nil {
		return nil, err
	}

	sharedSecret, ok := newSharedSecret(p.SharedSecret)
	if !ok {
		return nil, errors.New("panda: invalid shared secret in serialised key exchange")
	}

	kx := &KeyExchange{
		rand:         rand,
		meetingPlace: meetingPlace,
		status:       p.GetStatus(),
		sharedSecret: sharedSecret,
		serialised:   serialised,
		kxBytes:      p.KeyExchangeBytes,
		message1:     p.Message1,
		message2:     p.Message2,
	}

	copy(kx.key[:], p.Key)
	copy(kx.meeting1[:], p.Meeting1)
	copy(kx.meeting2[:], p.Meeting2)
	copy(kx.sharedKey[:], p.SharedKey)
	copy(kx.dhPrivate[:], p.DhPrivate)
	curve25519.ScalarBaseMult(&kx.dhPublic, &kx.dhPrivate)

	return kx, nil
}

func (kx *KeyExchange) Marshal() []byte {
	kx.Lock()
	defer kx.Unlock()

	return kx.serialised
}

func (kx *KeyExchange) updateSerialised() {
	p := &panda_proto.KeyExchange{
		Status:           kx.status.Enum(),
		SharedSecret:     kx.sharedSecret.toProto(),
		KeyExchangeBytes: kx.kxBytes,
	}
	if kx.status != panda_proto.KeyExchange_INIT {
		p.DhPrivate = kx.dhPrivate[:]
		p.Key = kx.key[:]
		p.Meeting1 = kx.meeting1[:]
		p.Meeting2 = kx.meeting2[:]
		p.Message1 = kx.message1
		p.Message2 = kx.message2
		p.SharedKey = kx.sharedKey[:]
	}
	serialised, err := proto.Marshal(p)
	if err != nil {
		panic(err)
	}

	kx.Lock()
	defer kx.Unlock()

	kx.serialised = serialised
}

func (kx *KeyExchange) shouldStop() bool {
	select {
	case <-kx.ShutdownChan:
		return true
	default:
		return false
	}

	panic("unreachable")
}

func (kx *KeyExchange) Run() ([]byte, error) {
	switch kx.status {
	case panda_proto.KeyExchange_INIT:
		if err := kx.derivePassword(); err != nil {
			return nil, err
		}
		kx.status = panda_proto.KeyExchange_EXCHANGE1
		kx.updateSerialised()
		kx.Log("password derivation complete.")
		if kx.shouldStop() {
			return nil, ShutdownErr
		}
		fallthrough
	case panda_proto.KeyExchange_EXCHANGE1:
		if err := kx.exchange1(); err != nil {
			return nil, err
		}
		kx.status = panda_proto.KeyExchange_EXCHANGE2
		kx.updateSerialised()
		kx.Log("first message exchange complete")
		if kx.shouldStop() {
			return nil, ShutdownErr
		}
		fallthrough
	case panda_proto.KeyExchange_EXCHANGE2:
		reply, err := kx.exchange2()
		if err != nil {
			return nil, err
		}
		return reply, nil
	default:
		panic("unknown state")
	}

	panic("unreachable")
}

func (kx *KeyExchange) derivePassword() error {
	serialised, err := proto.Marshal(kx.sharedSecret.toProto())
	if err != nil {
		return err
	}

	if kx.Testing || kx.sharedSecret.isStrongRandom() {
		h := hkdf.New(sha256.New, serialised, nil, []byte("PANDA strong secret expansion"))
		if _, err := h.Read(kx.key[:]); err != nil {
			return err
		}
		if _, err := h.Read(kx.meeting1[:]); err != nil {
			return err
		}
		if _, err := h.Read(kx.meeting2[:]); err != nil {
			return err
		}
	} else {
		var data []byte
		if runtime.GOARCH == "386" && runtime.GOOS == "linux" {
			// We're having GC problems on 32-bit systems with the
			// scrypt allocation. In order to help the GC out, the
			// scrypt computation is done in a subprocess.
			cmd := exec.Command("/proc/self/exe", "--panda-scrypt")
			var in, out bytes.Buffer
			binary.Write(&in, binary.LittleEndian, uint32(len(serialised)))
			in.Write(serialised)

			cmd.Stdin = &in
			cmd.Stdout = &out
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			data = out.Bytes()
			if len(data) != 32*3 {
				return errors.New("scrypt subprocess returned wrong number of bytes: " + strconv.Itoa(len(data)))
			}
		} else {
			if data, err = scrypt.Key(serialised, nil, 1<<17, 16, 4, 32*3); err != nil {
				return err
			}
		}

		copy(kx.key[:], data)
		copy(kx.meeting1[:], data[32:])
		copy(kx.meeting2[:], data[64:])
	}

	var encryptedDHPublic [32]byte
	rijndael.NewCipher(&kx.key).Encrypt(&encryptedDHPublic, &kx.dhPublic)

	l := len(encryptedDHPublic)
	if padding := kx.meetingPlace.Padding(); l > padding {
		return errors.New("panda: initial message too large for meeting place")
	} else if l < padding {
		l = padding
	}

	kx.message1 = make([]byte, l)
	copy(kx.message1, encryptedDHPublic[:])
	if _, err := io.ReadFull(kx.rand, kx.message1[len(encryptedDHPublic):]); err != nil {
		return err
	}

	return nil
}

func (kx *KeyExchange) exchange1() error {
	reply, err := kx.meetingPlace.Exchange(kx.Log, kx.meeting1[:], kx.message1[:], kx.ShutdownChan)
	if err != nil {
		return err
	}

	var peerDHPublic, encryptedPeerDHPublic [32]byte
	if len(reply) < len(encryptedPeerDHPublic) {
		return errors.New("panda: meeting point reply too small")
	}

	copy(encryptedPeerDHPublic[:], reply)
	rijndael.NewCipher(&kx.key).Decrypt(&peerDHPublic, &encryptedPeerDHPublic)

	curve25519.ScalarMult(&kx.sharedKey, &kx.dhPrivate, &peerDHPublic)

	paddedLen := kx.meetingPlace.Padding()
	padded := make([]byte, paddedLen-24 /* nonce */ -secretbox.Overhead)
	binary.LittleEndian.PutUint32(padded, uint32(len(kx.kxBytes)))
	copy(padded[4:], kx.kxBytes)
	if _, err := io.ReadFull(kx.rand, padded[4+len(kx.kxBytes):]); err != nil {
		return err
	}

	var nonce [24]byte
	if _, err := io.ReadFull(kx.rand, nonce[:]); err != nil {
		return err
	}

	kx.message2 = make([]byte, paddedLen)
	copy(kx.message2, nonce[:])
	secretbox.Seal(kx.message2[24:24], padded, &nonce, &kx.sharedKey)

	return nil
}

func (kx *KeyExchange) exchange2() ([]byte, error) {
	reply, err := kx.meetingPlace.Exchange(kx.Log, kx.meeting2[:], kx.message2[:], kx.ShutdownChan)
	if err != nil {
		return nil, err
	}

	var nonce [24]byte
	if len(reply) < len(nonce) {
		return nil, errors.New("panda: meeting point reply too small")
	}

	if kx.sharedKey[0] == 0 && kx.sharedKey[1] == 0 {
		panic("here")
	}
	copy(nonce[:], reply)
	message, ok := secretbox.Open(nil, reply[24:], &nonce, &kx.sharedKey)
	if !ok {
		return nil, errors.New("panda: peer's message cannot be authenticated")
	}

	if len(message) < 4 {
		return nil, errors.New("panda: peer's message is invalid")
	}
	l := binary.LittleEndian.Uint32(message)
	message = message[4:]
	if l > uint32(len(message)) {
		return nil, errors.New("panda: peer's message is truncated")
	}
	message = message[:int(l)]
	return message, nil
}