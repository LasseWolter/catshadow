// Package ratchet implements the axolotl ratchet, by Trevor Perrin. See
// https://github.com/trevp/axolotl/wiki.
package ratchet

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"time"

	"github.com/agl/ed25519"
	"github.com/agl/ed25519/extra25519"
	"github.com/ugorji/go/codec"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	// headerSize is the size, in bytes, of a header's plaintext contents.
	headerSize = 4 /* uint32 message count */ +
		4 /* uint32 previous message count */ +
		32 /* curve25519 ratchet public */ +
		24 /* nonce for message */
	// sealedHeader is the size, in bytes, of an encrypted header.
	sealedHeaderSize = 24 /* nonce */ + headerSize + secretbox.Overhead
	// nonceInHeaderOffset is the offset of the message nonce in the
	// header's plaintext.
	nonceInHeaderOffset = 4 + 4 + 32
	// maxMissingMessages is the maximum number of missing messages that
	// we'll keep track of.
	maxMissingMessages = 8

	RatchetKeyMaxLifetime = time.Hour * 672

	// DoubleRatchetOverhead is the number of bytes the ratchet adds in ciphertext overhead.
	DoubleRatchetOverhead = 120
)

var cborHandle = new(codec.CborHandle)

type KeyExchange struct {
	PublicKey      []byte
	IdentityPublic []byte
	Dh             []byte
	Dh1            []byte
}

type SignedKeyExchange struct {
	Signed    []byte
	Signature []byte
}

type MessageKey struct {
	Num          uint32
	Key          []byte
	CreationTime int64
}

type SavedKeys struct {
	HeaderKey   []byte
	MessageKeys []*MessageKey
}

type RatchetState struct {
	TheirSigningPublic  []byte
	TheirIdentityPublic []byte
	MySigningPublic     []byte
	MySigningPrivate    []byte
	MyIdentityPrivate   []byte
	MyIdentityPublic    []byte
	SavedKeys           []*SavedKeys
	RootKey             []byte
	SendHeaderKey       []byte
	RecvHeaderKey       []byte
	NextSendHeaderKey   []byte
	NextRecvHeaderKey   []byte
	SendChainKey        []byte
	RecvChainKey        []byte
	SendRatchetPrivate  []byte
	RecvRatchetPublic   []byte
	SendCount           uint32
	RecvCount           uint32
	PrevSendCount       uint32
	Private0            []byte
	Private1            []byte
	Ratchet             bool
}

// Ratchet contains the per-contact, crypto state.
type Ratchet struct {
	TheirSigningPublic  [32]byte
	TheirIdentityPublic [32]byte
	MySigningPublic     [32]byte
	MySigningPrivate    [64]byte
	MyIdentityPrivate   [32]byte
	MyIdentityPublic    [32]byte

	// Now is an optional function that will be used to get the current
	// time. If nil, time.Now is used.
	Now func() time.Time

	// rootKey gets updated by the DH ratchet.
	rootKey [32]byte
	// Header keys are used to encrypt message headers.
	sendHeaderKey, recvHeaderKey         [32]byte
	nextSendHeaderKey, nextRecvHeaderKey [32]byte
	// Chain keys are used for forward secrecy updating.
	sendChainKey, recvChainKey            [32]byte
	sendRatchetPrivate, recvRatchetPublic [32]byte
	sendCount, recvCount                  uint32
	prevSendCount                         uint32
	// ratchet is true if we will send a new ratchet value in the next message.
	ratchet bool

	// saved is a map from a header key to a map from sequence number to
	// message key.
	saved map[[32]byte]map[uint32]savedKey

	// kxPrivate0 and kxPrivate1 contain curve25519 private values during
	// the key exchange phase. They are not valid once key exchange has
	// completed.
	kxPrivate0, kxPrivate1 *[32]byte

	rand io.Reader
}

// savedKey contains a message key and timestamp for a message which has not
// been received. The timestamp comes from the message by which we learn of the
// missing message.
type savedKey struct {
	key       [32]byte
	timestamp time.Time
}

func (r *Ratchet) randBytes(buf []byte) {
	if _, err := io.ReadFull(r.rand, buf); err != nil {
		panic(err)
	}
}

func New(rand io.Reader) (*Ratchet, error) {
	r := &Ratchet{
		rand:       rand,
		kxPrivate0: new([32]byte),
		kxPrivate1: new([32]byte),
		saved:      make(map[[32]byte]map[uint32]savedKey),
	}
	r.randBytes(r.kxPrivate0[:])
	r.randBytes(r.kxPrivate1[:])
	mySigningPublic, mySigningPrivate, err := ed25519.GenerateKey(rand)
	if err != nil {
		return nil, err
	}
	r.MySigningPublic = *mySigningPublic
	r.MySigningPrivate = *mySigningPrivate
	extra25519.PrivateKeyToCurve25519(&r.MyIdentityPrivate, mySigningPrivate)
	curve25519.ScalarBaseMult(&r.MyIdentityPublic, &r.MyIdentityPrivate)

	// sanity math assertion
	var curve25519Public [32]byte
	extra25519.PublicKeyToCurve25519(&curve25519Public, &r.MySigningPublic)
	if !bytes.Equal(curve25519Public[:], r.MyIdentityPublic[:]) {
		panic("wtf")
	}

	return r, nil
}

func (r *Ratchet) CreateKeyExchange() (*SignedKeyExchange, error) {
	kx := &KeyExchange{
		PublicKey:      make([]byte, len(r.MySigningPublic[:])),
		IdentityPublic: make([]byte, len(r.TheirIdentityPublic[:])),
	}
	copy(kx.PublicKey, r.MySigningPublic[:])
	copy(kx.IdentityPublic, r.MyIdentityPublic[:])
	err := r.FillKeyExchange(kx)
	if err != nil {
		return nil, err
	}
	serialized := []byte{}
	enc := codec.NewEncoderBytes(&serialized, cborHandle)
	if err := enc.Encode(kx); err != nil {
		return nil, err
	}
	sig := ed25519.Sign(&r.MySigningPrivate, serialized)
	return &SignedKeyExchange{
		Signed:    serialized,
		Signature: sig[:],
	}, nil
}

// FillKeyExchange sets elements of kx with key exchange information from the
// ratchet.
func (r *Ratchet) FillKeyExchange(kx *KeyExchange) error {
	if r.kxPrivate0 == nil || r.kxPrivate1 == nil {
		return errors.New("ratchet: handshake already complete")
	}

	var public0, public1 [32]byte
	curve25519.ScalarBaseMult(&public0, r.kxPrivate0)
	curve25519.ScalarBaseMult(&public1, r.kxPrivate1)
	kx.Dh = public0[:]
	kx.Dh1 = public1[:]

	return nil
}

// deriveKey takes an HMAC object and a label and calculates out = HMAC(k, label).
func deriveKey(out *[32]byte, label []byte, h hash.Hash) {
	h.Reset()
	h.Write(label)
	n := h.Sum(out[:0])
	if &n[0] != &out[0] {
		panic("hash function too large")
	}
}

// These constants are used as the label argument to deriveKey to derive
// independent keys from a master key.
var (
	chainKeyLabel          = []byte("chain key")
	headerKeyLabel         = []byte("header key")
	nextRecvHeaderKeyLabel = []byte("next receive header key")
	rootKeyLabel           = []byte("root key")
	rootKeyUpdateLabel     = []byte("root key update")
	sendHeaderKeyLabel     = []byte("next send header key")
	messageKeyLabel        = []byte("message key")
	chainKeyStepLabel      = []byte("chain key step")
)

func (r *Ratchet) ProcessKeyExchange(signedKeyExchange *SignedKeyExchange) error {
	var sig [64]byte
	if len(signedKeyExchange.Signature) != len(sig) {
		return errors.New("invalid signature length")
	}
	copy(sig[:], signedKeyExchange.Signature)

	kx := new(KeyExchange)
	err := codec.NewDecoderBytes(signedKeyExchange.Signed, cborHandle).Decode(&kx)
	if err != nil {
		return err
	}
	if len(kx.PublicKey) != len(r.TheirSigningPublic) {
		return errors.New("invalid public key")
	}
	copy(r.TheirSigningPublic[:], kx.PublicKey)

	if !ed25519.Verify(&r.TheirSigningPublic, signedKeyExchange.Signed, &sig) {
		return errors.New("invalid signature")
	}

	var ed25519Public, curve25519Public [32]byte
	copy(ed25519Public[:], kx.PublicKey)
	extra25519.PublicKeyToCurve25519(&curve25519Public, &ed25519Public)
	if !bytes.Equal(curve25519Public[:], kx.IdentityPublic[:]) {
		return errors.New("ratchet: key exchange public key and identity public key must be isomorphically equal")
	}
	if len(kx.PublicKey) != len(r.TheirSigningPublic) {
		return errors.New("invalid public key")
	}
	copy(r.TheirSigningPublic[:], kx.PublicKey)
	if len(r.TheirIdentityPublic) != len(kx.IdentityPublic) {
		return errors.New("invalid public identity")
	}
	copy(r.TheirIdentityPublic[:], kx.IdentityPublic)
	return r.CompleteKeyExchange(kx)
}

// CompleteKeyExchange takes a KeyExchange message from the other party and
// establishes the ratchet.
func (r *Ratchet) CompleteKeyExchange(kx *KeyExchange) error {
	if r.kxPrivate0 == nil {
		return errors.New("ratchet: handshake already complete")
	}

	var public0 [32]byte
	curve25519.ScalarBaseMult(&public0, r.kxPrivate0)

	if len(kx.Dh) != len(public0) {
		return errors.New("ratchet: peer's key exchange is invalid")
	}
	if len(kx.Dh1) != len(public0) {
		return errors.New("ratchet: peer using old-form key exchange")
	}

	var amAlice bool
	switch bytes.Compare(public0[:], kx.Dh) {
	case -1:
		amAlice = true
	case 1:
		amAlice = false
	case 0:
		return errors.New("ratchet: peer echoed our own DH values back")
	}

	var theirDH [32]byte
	copy(theirDH[:], kx.Dh)

	keyMaterial := make([]byte, 0, 32*5)
	var sharedKey [32]byte
	curve25519.ScalarMult(&sharedKey, r.kxPrivate0, &theirDH)
	keyMaterial = append(keyMaterial, sharedKey[:]...)

	if amAlice {
		curve25519.ScalarMult(&sharedKey, &r.MyIdentityPrivate, &theirDH)
		keyMaterial = append(keyMaterial, sharedKey[:]...)
		curve25519.ScalarMult(&sharedKey, r.kxPrivate0, &r.TheirIdentityPublic)
		keyMaterial = append(keyMaterial, sharedKey[:]...)
	} else {
		curve25519.ScalarMult(&sharedKey, r.kxPrivate0, &r.TheirIdentityPublic)
		keyMaterial = append(keyMaterial, sharedKey[:]...)
		curve25519.ScalarMult(&sharedKey, &r.MyIdentityPrivate, &theirDH)
		keyMaterial = append(keyMaterial, sharedKey[:]...)
	}

	h := hmac.New(sha256.New, keyMaterial)
	deriveKey(&r.rootKey, rootKeyLabel, h)
	if amAlice {
		deriveKey(&r.recvHeaderKey, headerKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.nextRecvHeaderKey, nextRecvHeaderKeyLabel, h)
		deriveKey(&r.recvChainKey, chainKeyLabel, h)
		copy(r.recvRatchetPublic[:], kx.Dh1)
	} else {
		deriveKey(&r.sendHeaderKey, headerKeyLabel, h)
		deriveKey(&r.nextRecvHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, nextRecvHeaderKeyLabel, h)
		deriveKey(&r.sendChainKey, chainKeyLabel, h)
		copy(r.sendRatchetPrivate[:], r.kxPrivate1[:])
	}

	r.ratchet = amAlice
	r.kxPrivate0 = nil
	r.kxPrivate1 = nil

	return nil
}

// Encrypt acts like append() but appends an encrypted version of msg to out.
func (r *Ratchet) Encrypt(out, msg []byte) []byte {
	if r.ratchet {
		r.randBytes(r.sendRatchetPrivate[:])
		copy(r.sendHeaderKey[:], r.nextSendHeaderKey[:])

		var sharedKey, keyMaterial [32]byte
		curve25519.ScalarMult(&sharedKey, &r.sendRatchetPrivate, &r.recvRatchetPublic)
		sha := sha256.New()
		sha.Write(rootKeyUpdateLabel)
		sha.Write(r.rootKey[:])
		sha.Write(sharedKey[:])
		sha.Sum(keyMaterial[:0])
		h := hmac.New(sha256.New, keyMaterial[:])
		deriveKey(&r.rootKey, rootKeyLabel, h)
		deriveKey(&r.nextSendHeaderKey, sendHeaderKeyLabel, h)
		deriveKey(&r.sendChainKey, chainKeyLabel, h)
		r.prevSendCount, r.sendCount = r.sendCount, 0
		r.ratchet = false
	}

	h := hmac.New(sha256.New, r.sendChainKey[:])
	var messageKey [32]byte
	deriveKey(&messageKey, messageKeyLabel, h)
	deriveKey(&r.sendChainKey, chainKeyStepLabel, h)

	var sendRatchetPublic [32]byte
	curve25519.ScalarBaseMult(&sendRatchetPublic, &r.sendRatchetPrivate)
	var header [headerSize]byte
	var headerNonce, messageNonce [24]byte
	r.randBytes(headerNonce[:])
	r.randBytes(messageNonce[:])

	binary.LittleEndian.PutUint32(header[0:4], r.sendCount)
	binary.LittleEndian.PutUint32(header[4:8], r.prevSendCount)
	copy(header[8:], sendRatchetPublic[:])
	copy(header[nonceInHeaderOffset:], messageNonce[:])
	out = append(out, headerNonce[:]...)
	out = secretbox.Seal(out, header[:], &headerNonce, &r.sendHeaderKey)
	r.sendCount++
	return secretbox.Seal(out, msg, &messageNonce, &messageKey)
}

// trySavedKeys tries to decrypt ciphertext using keys saved for missing messages.
func (r *Ratchet) trySavedKeys(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < sealedHeaderSize {
		return nil, errors.New("ratchet: header too small to be valid")
	}

	sealedHeader := ciphertext[:sealedHeaderSize]
	var nonce [24]byte
	copy(nonce[:], sealedHeader)
	sealedHeader = sealedHeader[len(nonce):]

	for headerKey, messageKeys := range r.saved {
		header, ok := secretbox.Open(nil, sealedHeader, &nonce, &headerKey)
		if !ok {
			continue
		}
		if len(header) != headerSize {
			continue
		}
		msgNum := binary.LittleEndian.Uint32(header[:4])
		msgKey, ok := messageKeys[msgNum]
		if !ok {
			// This is a fairly common case: the message key might
			// not have been saved because it's the next message
			// key.
			return nil, nil
		}

		sealedMessage := ciphertext[sealedHeaderSize:]
		copy(nonce[:], header[nonceInHeaderOffset:])
		msg, ok := secretbox.Open(nil, sealedMessage, &nonce, &msgKey.key)
		if !ok {
			return nil, errors.New("ratchet: corrupt message")
		}
		delete(messageKeys, msgNum)
		if len(messageKeys) == 0 {
			delete(r.saved, headerKey)
		}
		return msg, nil
	}

	return nil, nil
}

// saveKeys takes a header key, the current chain key, a received message
// number and the expected message number and advances the chain key as needed.
// It returns the message key for given given message number and the new chain
// key. If any messages have been skipped over, it also returns savedKeys, a
// map suitable for merging with r.saved, that contains the message keys for
// the missing messages.
func (r *Ratchet) saveKeys(headerKey, recvChainKey *[32]byte, messageNum, receivedCount uint32) (provisionalChainKey, messageKey [32]byte, savedKeys map[[32]byte]map[uint32]savedKey, err error) {
	if messageNum < receivedCount {
		// This is a message from the past, but we didn't have a saved
		// key for it, which means that it's a duplicate message or we
		// expired the save key.
		err = errors.New("ratchet: duplicate message or message delayed longer than tolerance")
		return
	}

	missingMessages := messageNum - receivedCount
	if missingMessages > maxMissingMessages {
		err = errors.New("ratchet: message exceeds reordering limit")
		return
	}

	// messageKeys maps from message number to message key.
	var messageKeys map[uint32]savedKey
	var now time.Time
	if missingMessages > 0 {
		messageKeys = make(map[uint32]savedKey)
		if r.Now == nil {
			now = time.Now()
		} else {
			now = r.Now()
		}
	}

	copy(provisionalChainKey[:], recvChainKey[:])

	for n := receivedCount; n <= messageNum; n++ {
		h := hmac.New(sha256.New, provisionalChainKey[:])
		deriveKey(&messageKey, messageKeyLabel, h)
		deriveKey(&provisionalChainKey, chainKeyStepLabel, h)
		if n < messageNum {
			messageKeys[n] = savedKey{messageKey, now}
		}
	}

	if messageKeys != nil {
		savedKeys = make(map[[32]byte]map[uint32]savedKey)
		savedKeys[*headerKey] = messageKeys
	}

	return
}

// mergeSavedKeys takes a map of saved message keys from saveKeys and merges it
// into r.saved.
func (r *Ratchet) mergeSavedKeys(newKeys map[[32]byte]map[uint32]savedKey) {
	for headerKey, newMessageKeys := range newKeys {
		messageKeys, ok := r.saved[headerKey]
		if !ok {
			r.saved[headerKey] = newMessageKeys
			continue
		}

		for n, messageKey := range newMessageKeys {
			messageKeys[n] = messageKey
		}
	}
}

// isZeroKey returns true if key is all zeros.
func isZeroKey(key *[32]byte) bool {
	var x uint8
	for _, v := range key {
		x |= v
	}

	return x == 0
}

func (r *Ratchet) Decrypt(ciphertext []byte) ([]byte, error) {
	msg, err := r.trySavedKeys(ciphertext)
	if err != nil || msg != nil {
		return msg, err
	}

	sealedHeader := ciphertext[:sealedHeaderSize]
	sealedMessage := ciphertext[sealedHeaderSize:]
	var nonce [24]byte
	copy(nonce[:], sealedHeader)
	sealedHeader = sealedHeader[len(nonce):]

	header, ok := secretbox.Open(nil, sealedHeader, &nonce, &r.recvHeaderKey)
	ok = ok && !isZeroKey(&r.recvHeaderKey)
	if ok {
		if len(header) != headerSize {
			return nil, errors.New("ratchet: incorrect header size")
		}
		messageNum := binary.LittleEndian.Uint32(header[:4])
		provisionalChainKey, messageKey, savedKeys, err := r.saveKeys(&r.recvHeaderKey, &r.recvChainKey, messageNum, r.recvCount)
		if err != nil {
			return nil, err
		}

		copy(nonce[:], header[nonceInHeaderOffset:])
		msg, ok := secretbox.Open(nil, sealedMessage, &nonce, &messageKey)
		if !ok {
			return nil, errors.New("ratchet: corrupt message")
		}

		copy(r.recvChainKey[:], provisionalChainKey[:])
		r.mergeSavedKeys(savedKeys)
		r.recvCount = messageNum + 1
		return msg, nil
	}

	header, ok = secretbox.Open(nil, sealedHeader, &nonce, &r.nextRecvHeaderKey)
	if !ok {
		return nil, errors.New("ratchet: cannot decrypt")
	}
	if len(header) != headerSize {
		return nil, errors.New("ratchet: incorrect header size")
	}

	if r.ratchet {
		return nil, errors.New("ratchet: received message encrypted to next header key without ratchet flag set")
	}

	messageNum := binary.LittleEndian.Uint32(header[:4])
	prevMessageCount := binary.LittleEndian.Uint32(header[4:8])

	_, _, oldSavedKeys, err := r.saveKeys(&r.recvHeaderKey, &r.recvChainKey, prevMessageCount, r.recvCount)
	if err != nil {
		return nil, err
	}

	var dhPublic, sharedKey, rootKey, chainKey, keyMaterial [32]byte
	copy(dhPublic[:], header[8:])

	curve25519.ScalarMult(&sharedKey, &r.sendRatchetPrivate, &dhPublic)

	sha := sha256.New()
	sha.Write(rootKeyUpdateLabel)
	sha.Write(r.rootKey[:])
	sha.Write(sharedKey[:])

	var rootKeyHMAC hash.Hash
	sha.Sum(keyMaterial[:0])
	rootKeyHMAC = hmac.New(sha256.New, keyMaterial[:])
	deriveKey(&rootKey, rootKeyLabel, rootKeyHMAC)
	deriveKey(&chainKey, chainKeyLabel, rootKeyHMAC)

	provisionalChainKey, messageKey, savedKeys, err := r.saveKeys(&r.nextRecvHeaderKey, &chainKey, messageNum, 0)
	if err != nil {
		return nil, err
	}

	copy(nonce[:], header[nonceInHeaderOffset:])
	msg, ok = secretbox.Open(nil, sealedMessage, &nonce, &messageKey)
	if !ok {
		return nil, errors.New("ratchet: corrupt message")
	}

	copy(r.rootKey[:], rootKey[:])
	copy(r.recvChainKey[:], provisionalChainKey[:])
	copy(r.recvHeaderKey[:], r.nextRecvHeaderKey[:])
	deriveKey(&r.nextRecvHeaderKey, sendHeaderKeyLabel, rootKeyHMAC)
	for i := range r.sendRatchetPrivate {
		r.sendRatchetPrivate[i] = 0
	}
	copy(r.recvRatchetPublic[:], dhPublic[:])

	r.recvCount = messageNum + 1
	r.mergeSavedKeys(oldSavedKeys)
	r.mergeSavedKeys(savedKeys)
	r.ratchet = true

	return msg, nil
}

func dup(key *[32]byte) []byte {
	if key == nil {
		return nil
	}

	ret := make([]byte, 32)
	copy(ret, key[:])
	return ret
}

func (r *Ratchet) MarshalBinary() (data []byte, err error) {
	s := r.Marshal(time.Now(), RatchetKeyMaxLifetime)
	var serialized []byte
	enc := codec.NewEncoderBytes(&serialized, new(codec.CborHandle))
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return serialized, nil
}

func (r *Ratchet) Marshal(now time.Time, lifetime time.Duration) *RatchetState {
	s := &RatchetState{
		TheirSigningPublic:  dup(&r.TheirSigningPublic),
		TheirIdentityPublic: dup(&r.TheirIdentityPublic),
		MySigningPublic:     dup(&r.MySigningPublic),
		MySigningPrivate:    r.MySigningPrivate[:],
		MyIdentityPrivate:   dup(&r.MyIdentityPrivate),
		MyIdentityPublic:    dup(&r.MyIdentityPublic),
		RootKey:             dup(&r.rootKey),
		SendHeaderKey:       dup(&r.sendHeaderKey),
		RecvHeaderKey:       dup(&r.recvHeaderKey),
		NextSendHeaderKey:   dup(&r.nextSendHeaderKey),
		NextRecvHeaderKey:   dup(&r.nextRecvHeaderKey),
		SendChainKey:        dup(&r.sendChainKey),
		RecvChainKey:        dup(&r.recvChainKey),
		SendRatchetPrivate:  dup(&r.sendRatchetPrivate),
		RecvRatchetPublic:   dup(&r.recvRatchetPublic),
		SendCount:           r.sendCount,
		RecvCount:           r.recvCount,
		PrevSendCount:       r.prevSendCount,
		Private0:            dup(r.kxPrivate0),
		Private1:            dup(r.kxPrivate1),
		Ratchet:             r.ratchet,
	}

	for headerKey, messageKeys := range r.saved {
		keys := make([]*MessageKey, 0, len(messageKeys))
		for messageNum, savedKey := range messageKeys {
			if now.Sub(savedKey.timestamp) > lifetime {
				continue
			}
			keys = append(keys, &MessageKey{
				Num:          messageNum,
				Key:          dup(&savedKey.key),
				CreationTime: savedKey.timestamp.Unix(),
			})
		}
		s.SavedKeys = append(s.SavedKeys, &SavedKeys{
			HeaderKey:   dup(&headerKey),
			MessageKeys: keys,
		})
	}

	return s
}

func unmarshalKey(dst *[32]byte, src []byte) bool {
	if len(src) != 32 {
		return false
	}
	copy(dst[:], src)
	return true
}

var badSerialisedKeyLengthErr = errors.New("ratchet: bad serialised key length")

func (r *Ratchet) UnmarshalBinary(data []byte) error {
	state := RatchetState{}
	err := codec.NewDecoderBytes(data, cborHandle).Decode(&state)
	if err != nil {
		return err
	}
	return r.Unmarshal(&state)
}

func (r *Ratchet) Unmarshal(s *RatchetState) error {
	copy(r.MySigningPublic[:], s.MySigningPublic)
	copy(r.MySigningPrivate[:], s.MySigningPrivate)
	if !unmarshalKey(&r.rootKey, s.RootKey) ||
		!unmarshalKey(&r.TheirSigningPublic, s.TheirSigningPublic) ||
		!unmarshalKey(&r.TheirIdentityPublic, s.TheirIdentityPublic) ||
		!unmarshalKey(&r.MyIdentityPrivate, s.MyIdentityPrivate) ||
		!unmarshalKey(&r.MyIdentityPublic, s.MyIdentityPublic) ||
		!unmarshalKey(&r.sendHeaderKey, s.SendHeaderKey) ||
		!unmarshalKey(&r.recvHeaderKey, s.RecvHeaderKey) ||
		!unmarshalKey(&r.nextSendHeaderKey, s.NextSendHeaderKey) ||
		!unmarshalKey(&r.nextRecvHeaderKey, s.NextRecvHeaderKey) ||
		!unmarshalKey(&r.sendChainKey, s.SendChainKey) ||
		!unmarshalKey(&r.recvChainKey, s.RecvChainKey) ||
		!unmarshalKey(&r.sendRatchetPrivate, s.SendRatchetPrivate) ||
		!unmarshalKey(&r.recvRatchetPublic, s.RecvRatchetPublic) {
		return badSerialisedKeyLengthErr
	}

	r.sendCount = s.SendCount
	r.recvCount = s.RecvCount
	r.prevSendCount = s.PrevSendCount
	r.ratchet = s.Ratchet

	if len(s.Private0) > 0 {
		if !unmarshalKey(r.kxPrivate0, s.Private0) ||
			!unmarshalKey(r.kxPrivate1, s.Private1) {
			return badSerialisedKeyLengthErr
		}
	} else {
		r.kxPrivate0 = nil
		r.kxPrivate1 = nil
	}

	for _, saved := range s.SavedKeys {
		var headerKey [32]byte
		if !unmarshalKey(&headerKey, saved.HeaderKey) {
			return badSerialisedKeyLengthErr
		}
		messageKeys := make(map[uint32]savedKey)
		for _, messageKey := range saved.MessageKeys {
			var savedKey savedKey
			if !unmarshalKey(&savedKey.key, messageKey.Key) {
				return badSerialisedKeyLengthErr
			}
			savedKey.timestamp = time.Unix(messageKey.CreationTime, 0)
			messageKeys[messageKey.Num] = savedKey
		}

		r.saved[headerKey] = messageKeys
	}

	return nil
}
