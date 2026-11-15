// Copyright JAMF Software, LLC

package key

import (
	"encoding/binary"
	"io"
)

const (
	V2              uint8 = 2
	V2SeqLen              = 8
	V2CreateSeqLen        = 8
	V2SepLen              = 1 // null separator between user key and seqno
	V2KindLen             = 1
	V2SuffixLen           = V2SeqLen + V2CreateSeqLen + V2KindLen
	V2MaxUserKeyLen       = 1024
	V2KeyLen              = keyHeaderLen + 1 + V2MaxUserKeyLen + V2SepLen + V2SuffixLen
	keyV2BodyLen          = V2KeyLen - keyHeaderLen
)

type RecordKind byte

const (
	KindValue RecordKind = iota
	KindTombstone
)

// V2Len computes length of a V2 key.
func V2Len(userkeyLen int) int {
	return keyHeaderLen + 1 + userkeyLen + V2SepLen + V2SuffixLen
}

var V2MinKey = func() []byte {
	return []byte{0}
}

var V2MaxKey = func() []byte {
	maxKey := make([]byte, V2MaxUserKeyLen)
	for i := range maxKey {
		maxKey[i] = 0xFF
	}
	return maxKey
}

type keyV2 struct {
	keyType     Type
	key         []byte
	seqno       uint64
	createSeqno uint64
	kind        RecordKind
}

func (k *keyV2) Encode(writer io.Writer) (int, error) {
	// Layout: [keyType 1B] [userKey bytes] [0x00 separator 1B]
	// [inverted seqno 8B] [creation seqno 8B] [record kind 1B]
	// The null separator ensures correct lexicographic ordering between keys
	// where one user key is a prefix of another.
	buf := make([]byte, 1+len(k.key)+V2SepLen+V2SuffixLen)
	buf[0] = byte(k.keyType)
	copy(buf[1:], k.key)
	buf[1+len(k.key)] = 0x00 // null separator

	var seqBuf [V2SeqLen]byte
	binary.BigEndian.PutUint64(seqBuf[:], k.seqno)
	for i := range seqBuf {
		seqBuf[i] = ^seqBuf[i]
	}
	suffixStart := 1 + len(k.key) + V2SepLen
	copy(buf[suffixStart:], seqBuf[:])
	binary.BigEndian.PutUint64(buf[suffixStart+V2SeqLen:], k.createSeqno)
	buf[len(buf)-V2KindLen] = byte(k.kind)

	return writer.Write(buf)
}

func (k *keyV2) Decode(reader io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(reader, int64(keyV2BodyLen+1)))
	if err != nil {
		return err
	}
	decoded, err := decodeV2Raw(raw)
	if err != nil {
		return err
	}
	*k = decoded
	return nil
}

func decodeV2Raw(raw []byte) (keyV2, error) {
	if len(raw) < 1+V2SepLen+V2SuffixLen || len(raw) > keyV2BodyLen {
		return keyV2{}, ErrMalformedV2Key
	}
	kind := RecordKind(raw[len(raw)-V2KindLen])
	if kind != KindValue && kind != KindTombstone {
		return keyV2{}, ErrMalformedV2Key
	}
	separator := len(raw) - V2SuffixLen - V2SepLen
	if raw[separator] != 0 {
		return keyV2{}, ErrMalformedV2Key
	}

	var seqBuf [V2SeqLen]byte
	copy(seqBuf[:], raw[separator+V2SepLen:separator+V2SepLen+V2SeqLen])
	for i := range seqBuf {
		seqBuf[i] = ^seqBuf[i]
	}

	return keyV2{
		keyType:     Type(raw[0]),
		key:         raw[1:separator],
		seqno:       binary.BigEndian.Uint64(seqBuf[:]),
		createSeqno: binary.BigEndian.Uint64(raw[separator+V2SepLen+V2SeqLen : len(raw)-V2KindLen]),
		kind:        kind,
	}, nil
}

// DecodeV2Seqno recovers the uint64 modification seqno from a physical V2 key.
// Returns 0 for non-V2 keys or malformed keys.
func DecodeV2Seqno(physicalKey []byte) uint64 {
	if len(physicalKey) <= keyHeaderLen || physicalKey[0] != V2 {
		return 0
	}
	k, err := decodeV2Raw(physicalKey[keyHeaderLen:])
	if err != nil {
		return 0
	}
	return k.seqno
}

// DecodeV2CreateSeqno recovers the creation seqno from a physical V2 key.
// Returns 0 for non-V2 keys or malformed keys.
func DecodeV2CreateSeqno(physicalKey []byte) uint64 {
	if len(physicalKey) <= keyHeaderLen || physicalKey[0] != V2 {
		return 0
	}
	k, err := decodeV2Raw(physicalKey[keyHeaderLen:])
	if err != nil {
		return 0
	}
	return k.createSeqno
}
