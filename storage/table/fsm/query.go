// Copyright JAMF Software, LLC

package fsm

import (
	"encoding/binary"
	"errors"
	"io"
	"iter"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/armadakv/armada/util/iterx"
	"github.com/cockroachdb/pebble/v2"
)

// commandIncrementalSnapshot streams only the changes (puts and deletes) with
// seqno > sinceIndex into w, encoded as proto Commands — identical wire format
// to commandSnapshot so the same chunked framing on the receiver side works.
//
// The function walks the raw physical V2 key space (all MVCC versions) and for
// each distinct user key emits the latest version whose seqno > sinceIndex:
//   - live value  → Command_PUT
//   - tombstone   → Command_DELETE
//
// Keys whose latest version has seqno <= sinceIndex are skipped entirely —
// they have not changed since the follower's last snapshot.
//
// System keys are always skipped; the caller is responsible for checking that
// sinceIndex > gcHorizon before calling (otherwise compacted versions may be
// missing and the delta would be incomplete).
func commandIncrementalSnapshot(reader pebble.Reader, tableName string, sinceIndex uint64, w io.Writer, stopc <-chan struct{}) (uint64, error) {
	iter, err := reader.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	idx, err := readLocalIndex(reader, sysLocalIndex)
	if err != nil {
		return 0, err
	}

	var buffer []byte
	for iter.First(); iter.Valid(); {
		select {
		case <-stopc:
			return 0, sm.ErrSnapshotStopped
		default:
			k, err := key.DecodeBytes(iter.Key())
			if err != nil {
				return 0, err
			}

			// Only consider user keys; advance past all system keys.
			if k.KeyType != key.TypeUser {
				iter.Next()
				continue
			}

			currentKey := make([]byte, len(iter.Key()))
			copy(currentKey, iter.Key())

			// The seqno of this physical key is the MVCC version (= leaderIndex
			// at the time of the write). Because higher seqnos sort first within
			// a prefix, iter.First() / SeekGE always lands on the latest version,
			// so the very first key we see for this user-key prefix is the one
			// we need to inspect.
			seqno := key.DecodeV2Seqno(iter.Key())

			if seqno > sinceIndex {
				// This key changed after sinceIndex — emit it.
				if isTombstone(iter.Value()) {
					buffer, err = writeDeleteCommand(tableName, k.Key, buffer)
				} else {
					buffer, err = writeCommand(tableName, k.Key, iter.Value(), buffer)
				}
				if err != nil {
					return 0, err
				}
				if _, err := w.Write(buffer); err != nil {
					return 0, err
				}
			}

			// Skip all remaining MVCC versions of this user key.
			if !iterNextUserKey(iter, currentKey) {
				break
			}
		}
	}
	return idx, nil
}

// writeDeleteCommand writes a DELETE proto.Command for key into (optionally provided) buffer.
func writeDeleteCommand(tableName string, userKey []byte, buffer []byte) ([]byte, error) {
	cmd := armadapb.CommandFromVTPool()
	defer cmd.ReturnToVTPool()
	cmd.Table = []byte(tableName)
	cmd.Type = armadapb.Command_DELETE
	cmd.Kv = &armadapb.KeyValue{
		Key: userKey,
	}
	size := cmd.SizeVT()
	if cap(buffer) < size {
		buffer = make([]byte, size*2)
	}
	n, err := cmd.MarshalToSizedBufferVT(buffer[:size])
	if err != nil {
		return buffer, err
	}
	return buffer[:n], err
}

const maxRangeSize uint64 = (4 * 1024 * 1024) - 1024 // 4MiB - 1KiB sentinel.

func commandSnapshot(reader pebble.Reader, tableName string, w io.Writer, stopc <-chan struct{}) (uint64, error) {
	iter, err := reader.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	idx, err := readLocalIndex(reader, sysLocalIndex)
	if err != nil {
		return 0, err
	}

	var buffer []byte
	for iter.First(); iter.Valid(); {
		select {
		case <-stopc:
			return 0, sm.ErrSnapshotStopped
		default:
			k, err := key.DecodeBytes(iter.Key())
			if err != nil {
				return 0, err
			}
			if k.KeyType == key.TypeUser {
				// Skip tombstoned keys — they have been deleted and must not appear in snapshots.
				currentKey := make([]byte, len(iter.Key()))
				copy(currentKey, iter.Key())
				if isTombstone(iter.Value()) {
					iterNextUserKey(iter, currentKey)
					continue
				}
				buffer, err = writeCommand(tableName, k.Key, iter.Value(), buffer)
				if err != nil {
					return 0, err
				}
				if _, err := w.Write(buffer); err != nil {
					return 0, err
				}
				// Skip older MVCC versions of this user key (latest version first).
				// iterNextUserKey is used instead of NextPrefix because NextPrefix
				// is disallowed when the iterator has a versioned MVCC upper bound.
				iterNextUserKey(iter, currentKey)
			} else {
				iter.Next()
			}
		}
	}
	return idx, nil
}

// writeCommand writes KV pair as PUT proto.Command into (optionally provided) buffer.
func writeCommand(tableName string, key []byte, val []byte, buffer []byte) ([]byte, error) {
	cmd := armadapb.CommandFromVTPool()
	defer cmd.ReturnToVTPool()
	cmd.Table = []byte(tableName)
	cmd.Type = armadapb.Command_PUT
	cmd.Kv = &armadapb.KeyValue{
		Key:   key,
		Value: val,
	}
	size := cmd.SizeVT()
	if cap(buffer) < size {
		buffer = make([]byte, size*2)
	}
	n, err := cmd.MarshalToSizedBufferVT(buffer[:size])
	if err != nil {
		return buffer, err
	}
	return buffer[:n], err
}

func readLocalIndex(db pebble.Reader, indexKey []byte) (idx uint64, err error) {
	indexVal, closer, err := db.Get(indexKey)
	if err != nil {
		if !errors.Is(err, pebble.ErrNotFound) {
			return 0, err
		}
		return 0, nil
	}

	defer func() {
		err = closer.Close()
	}()

	return binary.LittleEndian.Uint64(indexVal), nil
}

func lookup(reader pebble.Reader, req *armadapb.RequestOp_Range) (*armadapb.ResponseOp_Range, error) {
	if req.RangeEnd != nil {
		return rangeLookup(reader, req)
	}
	return singleLookup(reader, req)
}

func rangeLookup(reader pebble.Reader, req *armadapb.RequestOp_Range) (*armadapb.ResponseOp_Range, error) {
	it, err := iterate(reader, req)
	if err != nil {
		return nil, err
	}
	return iterx.First(it), nil
}

func singleLookup(reader pebble.Reader, req *armadapb.RequestOp_Range) (*armadapb.ResponseOp_Range, error) {
	keyBuf := bufferPool.Get()
	defer bufferPool.Put(keyBuf)

	err := encodeUserKey(keyBuf, req.Key, ^uint64(0))
	if err != nil {
		return nil, err
	}

	iter, err := reader.NewIter(nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = iter.Close()
	}()
	found := iter.SeekPrefixGE(keyBuf.Bytes())
	// SeekPrefixGE with the V2 split function lands on the latest version of the key
	// (highest seqno sorts first due to bit-inversion encoding).
	if !found {
		return &armadapb.ResponseOp_Range{}, nil
	}

	// If the latest version is a tombstone the key has been deleted.
	if isTombstone(iter.Value()) {
		return &armadapb.ResponseOp_Range{}, nil
	}

	kv := &armadapb.KeyValue{Key: req.Key}
	value := iter.Value()
	if !req.KeysOnly && !req.CountOnly && len(value) > 0 {
		kv.Value = make([]byte, len(value))
		copy(kv.Value, value)
	}

	var kvs []*armadapb.KeyValue
	if !req.CountOnly {
		kvs = append(kvs, kv)
	}

	return &armadapb.ResponseOp_Range{
		Kvs:   kvs,
		Count: 1,
	}, nil
}

func iteratorLookup(reader pebble.Reader, req *armadapb.RequestOp_Range) (iter.Seq[*armadapb.ResponseOp_Range], error) {
	if req.RangeEnd != nil {
		return iterate(reader, req)
	}
	single, err := singleLookup(reader, req)
	if err != nil {
		return nil, err
	}
	return iterx.From(single), nil
}

// IteratorRequest returns open pebble.Iterator it is an API consumer responsibility to close it.
type IteratorRequest struct {
	RangeOp *armadapb.RequestOp_Range
}

// SnapshotRequest to write Command snapshot into provided writer.
type SnapshotRequest struct {
	Writer  io.Writer
	Stopper <-chan struct{}
}

// IncrementalSnapshotRequest to write an incremental Command snapshot into provided writer.
// Only changes (puts and deletes) with seqno > SinceIndex are emitted.
// The caller must ensure SinceIndex > gcHorizon, otherwise the delta may be incomplete.
type IncrementalSnapshotRequest struct {
	Writer     io.Writer
	Stopper    <-chan struct{}
	SinceIndex uint64
}

// SnapshotResponse returns local index to which the snapshot was created.
type SnapshotResponse struct {
	Index uint64
}

// LocalIndexRequest to read local index.
type LocalIndexRequest struct{}

// LeaderIndexRequest to read leader index.
type LeaderIndexRequest struct{}

// IndexResponse returns local index.
type IndexResponse struct {
	Index uint64
}

// PathRequest request data disk paths.
type PathRequest struct{}

// PathResponse returns SM data paths.
type PathResponse struct {
	Path string
}

// GCHorizonRequest reads the current GC horizon from the FSM. The returned
// IndexResponse.Index is the highest Raft index at which a Command_GC has been
// applied; any MVCC revision strictly below this value may have been reclaimed.
type GCHorizonRequest struct{}
