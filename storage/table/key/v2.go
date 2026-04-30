// Copyright JAMF Software, LLC

package key

import (
	"encoding/binary"
	"io"
)

const (
	V2           uint8 = 2
	V2SeqLen           = 8
	V2SepLen           = 1 // null separator between user key and seqno
	V2KeyLen           = 1024
	keyV2BodyLen       = V2KeyLen - keyHeaderLen
)

// V2Len computes length of a V2 key.
func V2Len(userkeyLen int) int {
	return keyHeaderLen + 1 + userkeyLen + V2SepLen + V2SeqLen
}

var V2MinKey = func() []byte {
	minKey := make([]byte, keyV2BodyLen-1-V2SepLen-V2SeqLen)
	return minKey
}

var V2MaxKey = func() []byte {
	maxKey := make([]byte, keyV2BodyLen-1-V2SepLen-V2SeqLen)
	for i := range maxKey {
		maxKey[i] = 0xFF
	}
	return maxKey
}

type keyV2 struct {
	keyType Type
	key     []byte
	seqno   uint64
}

func (k *keyV2) Encode(writer io.Writer) (int, error) {
	// Layout: [keyType 1B] [userKey bytes] [0x00 separator 1B] [seqno 8B]
	// The null separator ensures correct lexicographic ordering between keys
	// where one user key is a prefix of another.
	buf := make([]byte, 1+len(k.key)+V2SepLen+V2SeqLen)
	buf[0] = byte(k.keyType)
	copy(buf[1:], k.key)
	buf[1+len(k.key)] = 0x00 // null separator

	var seqBuf [V2SeqLen]byte
	binary.BigEndian.PutUint64(seqBuf[:], k.seqno)
	for i := range seqBuf {
		seqBuf[i] = ^seqBuf[i]
	}
	copy(buf[1+len(k.key)+V2SepLen:], seqBuf[:])

	return writer.Write(buf)
}

func (k *keyV2) Decode(reader io.Reader) error {
	// Layout: [keyType 1B] [userKey bytes] [0x00 separator 1B] [seqno 8B]
	// We read everything and then split off the last (V2SepLen + V2SeqLen) bytes.
	raw, err := io.ReadAll(io.LimitReader(reader, int64(keyV2BodyLen)))
	if err != nil {
		return err
	}
	if len(raw) < 1+V2SepLen+V2SeqLen {
		return ErrMissingKeyType
	}
	k.keyType = Type(raw[0])
	// The last V2SeqLen bytes are the encoded seqno; before that is the null separator.
	seqStart := len(raw) - V2SeqLen
	k.key = raw[1 : seqStart-V2SepLen]

	var seqBuf [V2SeqLen]byte
	copy(seqBuf[:], raw[seqStart:])
	for i := range seqBuf {
		seqBuf[i] = ^seqBuf[i]
	}
	k.seqno = binary.BigEndian.Uint64(seqBuf[:])
	return nil
}

// DecodeV2Seqno recovers the uint64 seqno (= leaderIndex at write time) from
// the last V2SeqLen bytes of a physical V2 Pebble key. Returns 0 for non-V2
// keys or keys that are too short.
func DecodeV2Seqno(physicalKey []byte) uint64 {
	if len(physicalKey) <= V2SeqLen || physicalKey[0] != V2 {
		return 0
	}
	var buf [V2SeqLen]byte
	copy(buf[:], physicalKey[len(physicalKey)-V2SeqLen:])
	for i := range buf {
		buf[i] = ^buf[i]
	}
	return binary.BigEndian.Uint64(buf[:])
}

func v2DecodeRaw(raw []byte) keyV2 {
	// Layout: [keyType 1B] [userKey bytes] [0x00 separator 1B] [seqno 8B]
	k := keyV2{}
	if len(raw) < 1+V2SepLen+V2SeqLen {
		return k
	}
	k.keyType = Type(raw[0])
	seqStart := len(raw) - V2SeqLen
	k.key = raw[1 : seqStart-V2SepLen]

	var seqBuf [V2SeqLen]byte
	copy(seqBuf[:], raw[seqStart:])
	for i := range seqBuf {
		seqBuf[i] = ^seqBuf[i]
	}
	k.seqno = binary.BigEndian.Uint64(seqBuf[:])
	return k
}
