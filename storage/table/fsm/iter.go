// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"iter"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
)

// iterNextUserKey advances piter past all MVCC versions of the current user key,
// positioning it on the first entry of the next distinct user key (or exhausting it).
// This is equivalent to NextPrefix but works even when NextPrefix is disallowed
// (e.g. when an upper bound is a versioned MVCC key).
//
// The strategy: build a seek key that is the current user-key prefix with the
// maximum possible seqno suffix (all 0xFF bytes), then call Next() once.  That
// positions piter on the very first key whose prefix is strictly greater than
// the current one.
func iterNextUserKey(piter *pebble.Iterator, currentPebbleKey []byte) bool {
	// The split point separates user-key prefix from the seqno suffix.
	// For V2 keys split = len(key) - V2SeqLen; for others split = len(key).
	split := key.V2SeqLen + key.V2SepLen + 1 // header(4) + keyType(1) accounted for below
	_ = split

	if len(currentPebbleKey) <= key.V2SeqLen || currentPebbleKey[0] != key.V2 {
		// Not a V2 key – plain Next() is sufficient (no MVCC versions).
		return piter.Next()
	}

	// For a V2 key the prefix (everything except the last V2SeqLen bytes) is
	// header + keyType + userKey + separator.  Build a seek target that is
	// identical to that prefix but with the seqno bytes set to 0xFF…FF, which
	// is the encoded form of seqno=0 and sorts last within the prefix.
	prefixLen := len(currentPebbleKey) - key.V2SeqLen
	seekKey := make([]byte, len(currentPebbleKey))
	copy(seekKey, currentPebbleKey[:prefixLen])
	for i := prefixLen; i < len(seekKey); i++ {
		seekKey[i] = 0xFF
	}

	// SeekGE on seekKey lands on seekKey itself (last version of current key)
	// or already past it.  One more Next() moves us to the first key of the
	// next user-key prefix.
	if !piter.SeekGE(seekKey) {
		return false
	}
	// If we landed exactly on seekKey (the last-version sentinel), advance once.
	if bytes.Equal(piter.Key(), seekKey) {
		return piter.Next()
	}
	// We already moved past all versions of the current user key.
	return true
}

// iterate until the provided pebble.Iterator is no longer valid or the limit is reached.
// Apply a function on the key/value pair in every iteration filling proto.RangeResponse.
func iterate(reader pebble.Reader, req *armadapb.RequestOp_Range) (iter.Seq[*armadapb.ResponseOp_Range], error) { //nolint:gocognit
	opts, err := iterOptionsForBounds(req.Key, req.RangeEnd)
	if err != nil {
		return nil, err
	}
	fill, sf := iterFuncsFromReq(req)
	limit := int(req.Limit)

	return func(yield func(*armadapb.ResponseOp_Range) bool) {
		piter, err := reader.NewIter(opts)
		if err != nil {
			return
		}
		defer func() {
			_ = piter.Close()
		}()
		response := &armadapb.ResponseOp_Range{}
		// If no results found yield and end immediately.
		if !piter.First() {
			yield(response)
			return
		}
		i := 0
		for {
			k, err := key.DecodeBytes(piter.Key())
			if err != nil {
				panic(err)
			}
			currentKey := make([]byte, len(piter.Key()))
			copy(currentKey, piter.Key())
			// Skip tombstoned keys — they represent MVCC deletes and must not
			// appear in range query results.
			if isTombstone(piter.Value()) {
				if !iterNextUserKey(piter, currentKey) {
					yield(response)
					return
				}
				continue
			}
			if i == limit && limit != 0 {
				response.More = piter.Next()
				yield(response)
				return
			}
			if (uint64(response.SizeVT()) + sf(k.Key, piter.ValueAndErr)) >= maxRangeSize {
				response.More = true
				if !yield(response) {
					return
				}
				response = &armadapb.ResponseOp_Range{}
			}
			i++
			fill(k.Key, piter.ValueAndErr, response)
			// iterNextUserKey skips all older MVCC versions of the same user key,
			// positioning the iterator on the first entry of the next distinct
			// user key prefix.  This works even when NextPrefix is disallowed
			// by pebble (e.g. when the upper bound is a versioned MVCC key).
			if !iterNextUserKey(piter, currentKey) {
				yield(response)
				return
			}
		}
	}, nil
}

func iterOptionsForBounds(low, high []byte) (*pebble.IterOptions, error) { //nolint:unparam
	lowBuf := bufferPool.Get()
	defer bufferPool.Put(lowBuf)
	// Use MaxUint64 seqno so the lower bound starts at the latest version of the low key
	// (MaxUint64 encodes as all-zeros after bit-inversion, which sorts first within a prefix).
	if err := encodeUserKey(lowBuf, low, ^uint64(0)); err != nil {
		return nil, err
	}
	iterOptions := &pebble.IterOptions{
		LowerBound: make([]byte, lowBuf.Len()),
	}
	copy(iterOptions.LowerBound, lowBuf.Bytes())

	if bytes.Equal(high, wildcard) {
		// In order to include the last key in the iterator as well we have to increment the rightmost byte of the maximum user key.
		iterOptions.UpperBound = make([]byte, len(maxUserKey))
		copy(iterOptions.UpperBound, maxUserKey)
		iterOptions.UpperBound = incrementRightmostByte(iterOptions.UpperBound)
	} else {
		highBuf := bufferPool.Get()
		defer bufferPool.Put(highBuf)

		// Use MaxUint64 seqno so the upper bound starts at the latest version of the high key,
		// making the range exclusive of the high key's latest version and all older versions.
		if err := encodeUserKey(highBuf, high, ^uint64(0)); err != nil {
			return nil, err
		}
		iterOptions.UpperBound = make([]byte, highBuf.Len())
		copy(iterOptions.UpperBound, highBuf.Bytes())
	}

	return iterOptions, nil
}

type lazyValueOrErr func() ([]byte, error)

// fillEntriesFunc fills proto.RangeResponse response.
type fillEntriesFunc func(key []byte, value lazyValueOrErr, response *armadapb.ResponseOp_Range)

// sizeEntriesFunc estimates entry size.
type sizeEntriesFunc func(key []byte, value lazyValueOrErr) uint64

func iterFuncsFromReq(req *armadapb.RequestOp_Range) (fillEntriesFunc, sizeEntriesFunc) {
	switch {
	case req.KeysOnly:
		return addKeyOnly, sizeKeyOnly
	case req.CountOnly:
		return addCountOnly, sizeCountOnly
	default:
		return addKVPair, sizeKVPair
	}
}

// addKVPair adds a key/value pair from the provided iterator to the proto.RangeResponse.
func addKVPair(key []byte, value lazyValueOrErr, response *armadapb.ResponseOp_Range) {
	val, _ := value()
	kv := &armadapb.KeyValue{Key: make([]byte, len(key)), Value: make([]byte, len(val))}
	copy(kv.Key, key)
	copy(kv.Value, val)
	response.Kvs = append(response.Kvs, kv)
	response.Count = int64(len(response.Kvs))
}

// sizeKVPair takes the full pair size into consideration.
func sizeKVPair(key []byte, value lazyValueOrErr) uint64 {
	val, _ := value()
	return uint64(len(key) + len(val))
}

// addKeyOnly adds a key from the provided iterator to the proto.RangeResponse.
func addKeyOnly(key []byte, _ lazyValueOrErr, response *armadapb.ResponseOp_Range) {
	kv := &armadapb.KeyValue{Key: make([]byte, len(key))}
	copy(kv.Key, key)
	response.Kvs = append(response.Kvs, kv)
	response.Count++
}

// sizeKeyOnly takes only the key into consideration.
func sizeKeyOnly(key []byte, _ lazyValueOrErr) uint64 {
	return uint64(len(key))
}

// addCountOnly increments number of keys from the provided iterator to the proto.RangeResponse.
func addCountOnly(_ []byte, _ lazyValueOrErr, response *armadapb.ResponseOp_Range) {
	response.Count++
}

// sizeCountOnly for count the size remains constant.
func sizeCountOnly(_ []byte, _ lazyValueOrErr) uint64 {
	return uint64(0)
}
