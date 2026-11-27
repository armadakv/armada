// Copyright Armada Contributors

package fsm

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/armadakv/armada/armadapb"
	rp "github.com/armadakv/armada/pebble"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
)

// WatchScanRequest reads the events of a key range in a window of revisions
// back from the MVCC store.
type WatchScanRequest struct {
	// Key and RangeEnd select the keys: a nil RangeEnd selects Key only, and
	// RangeEnd "\x00" every key at or above Key.
	Key      []byte
	RangeEnd []byte
	// From and To are the first and the last revision of the window.
	From uint64
	To   uint64
	// PrevKV reads the previous key-value of each event, when it still exists.
	PrevKV bool
	// MaxBytes bounds the size of the returned events. The window is cut
	// after the last revision that fits; at least one revision with events is
	// always returned. Zero means no bound.
	MaxBytes int
}

// WatchScanResponse holds the events read by a WatchScanRequest.
type WatchScanResponse struct {
	// Events are the events of [From, Through], ordered by revision and key.
	Events []*armadapb.Event
	// Through is the last revision covered by Events. It is To unless
	// MaxBytes cut the window short.
	Through uint64
	// Floor is the history floor of the scanned state: the events of
	// revisions at or below it are not available.
	Floor uint64
	// Compacted is set when From is at or below Floor. No events are returned.
	Compacted bool
}

// watchEventOverhead approximates the encoding overhead of one event beyond
// its key and value bytes.
const watchEventOverhead = 32

// historyFloor returns the highest revision whose events are not available
// in the given state. Garbage collection removes the versions below the GC
// horizon that a newer version shadows.
func historyFloor(reader pebble.Reader) (uint64, error) {
	return readLocalIndex(reader, sysGCHorizon)
}

// watchScan reads the events of a window of revisions from one consistent
// view of the store. Every physical version of a key is one event: a live
// value is a PUT and a tombstone is a DELETE, and the next older version of
// the same key is its previous key-value. That matches what the apply path
// records, except that a previous version below the GC horizon may already
// be gone.
func watchScan(reader pebble.Reader, req WatchScanRequest) (*WatchScanResponse, error) {
	floor, err := historyFloor(reader)
	if err != nil {
		return nil, err
	}
	res := &WatchScanResponse{Floor: floor, Through: req.To}
	if req.From <= floor {
		res.Compacted = true
		res.Through = 0
		return res, nil
	}
	if req.To < req.From {
		res.Through = req.From - 1
		return res, nil
	}

	through, found, err := watchScanCut(reader, req)
	if err != nil {
		return nil, err
	}
	res.Through = through
	if !found {
		return res, nil
	}
	res.Events, err = watchScanEvents(reader, req, through)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// watchScanCut sizes the events of the window without reading values and
// returns the last revision to read. It reports whether the window has any
// events at all.
func watchScanCut(reader pebble.Reader, req WatchScanRequest) (uint64, bool, error) {
	iter, err := watchScanIter(reader, req, req.To, false)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = iter.Close() }()

	sizes := make(map[uint64]int)
	total := 0
	for valid := iter.First(); valid; {
		k, err := key.DecodeBytes(iter.Key())
		if err != nil {
			return 0, false, err
		}
		switch {
		case k.KeyType != key.TypeUser:
			valid = iter.Next()
			continue
		case k.Seqno < req.From:
			// Versions are newest first, so the rest of this key is older.
			valid = iterNextUserKey(iter, nil)
			continue
		case k.Seqno <= req.To:
			lv := iter.LazyValue()
			n := len(k.Key) + lv.Len() + watchEventOverhead
			sizes[k.Seqno] += n
			total += n
		}
		valid = iter.Next()
	}
	if err := iter.Error(); err != nil {
		return 0, false, err
	}
	if len(sizes) == 0 {
		return req.To, false, nil
	}
	if req.MaxBytes <= 0 || total <= req.MaxBytes {
		return req.To, true, nil
	}

	revs := make([]uint64, 0, len(sizes))
	for r := range sizes {
		revs = append(revs, r)
	}
	slices.Sort(revs)
	acc := 0
	for i, r := range revs {
		acc += sizes[r]
		if i+1 < len(revs) && acc+sizes[revs[i+1]] > req.MaxBytes {
			// Revisions between this one and the next have no events.
			return revs[i+1] - 1, true, nil
		}
	}
	return req.To, true, nil
}

// watchScanEvents reads the events of [req.From, through].
func watchScanEvents(reader pebble.Reader, req WatchScanRequest, through uint64) ([]*armadapb.Event, error) {
	iter, err := watchScanIter(reader, req, through, req.PrevKV)
	if err != nil {
		return nil, err
	}
	defer func() { _ = iter.Close() }()

	var (
		events []*armadapb.Event
		// pending is the last event of the current key; the next older
		// version of the same key is its previous key-value.
		pending    *armadapb.Event
		pendingKey []byte
	)
	for valid := iter.First(); valid; {
		k, err := key.DecodeBytes(iter.Key())
		if err != nil {
			return nil, err
		}
		if k.KeyType != key.TypeUser {
			valid = iter.Next()
			continue
		}
		if pending != nil && !bytes.Equal(pendingKey, k.Key) {
			pending = nil
		}
		if pending != nil {
			if !isTombstone(k) {
				val, err := iter.ValueAndErr()
				if err != nil {
					return nil, err
				}
				pending.PrevKv, err = makeKeyValue(k.Key, val, k.CreateSeqno, k.Seqno)
				if err != nil {
					return nil, err
				}
			}
			pending = nil
		}
		switch {
		case k.Seqno < req.From:
			valid = iterNextUserKey(iter, nil)
			continue
		case k.Seqno <= through:
			if err := validateRevision(k.Seqno); err != nil {
				return nil, err
			}
			ev := &armadapb.Event{Type: armadapb.Event_PUT}
			var val []byte
			if isTombstone(k) {
				ev.Type = armadapb.Event_DELETE
			} else if val, err = iter.ValueAndErr(); err != nil {
				return nil, err
			}
			ev.Kv = &armadapb.KeyValue{Key: bytes.Clone(k.Key), CreateRevision: int64(k.CreateSeqno), ModRevision: int64(k.Seqno)}
			if ev.Type == armadapb.Event_PUT {
				ev.Kv.Value = bytes.Clone(val)
			}
			events = append(events, ev)
			if req.PrevKV {
				pending = ev
				pendingKey = ev.Kv.Key
			}
		}
		valid = iter.Next()
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	slices.SortFunc(events, func(a, b *armadapb.Event) int {
		if c := cmp.Compare(a.Kv.ModRevision, b.Kv.ModRevision); c != 0 {
			return c
		}
		return bytes.Compare(a.Kv.Key, b.Kv.Key)
	})
	return events, nil
}

// watchScanIter returns an iterator over the versions of the requested keys.
// Unless older versions are needed for previous key-values, SST blocks that
// hold no version of the window are skipped.
func watchScanIter(reader pebble.Reader, req WatchScanRequest, through uint64, withOlder bool) (*pebble.Iterator, error) {
	var opts *pebble.IterOptions
	if req.RangeEnd == nil {
		lower := bufferPool.Get()
		defer bufferPool.Put(lower)
		if err := encodeUserKeyPrefix(lower, req.Key); err != nil {
			return nil, err
		}
		lower.Truncate(lower.Len() - key.V2SuffixLen)
		// All versions of a key lie below its prefix with the separator
		// replaced by 0x01.
		upper := bytes.Clone(lower.Bytes())
		upper[len(upper)-1] = 1
		opts = &pebble.IterOptions{LowerBound: bytes.Clone(lower.Bytes()), UpperBound: upper}
	} else {
		var err error
		if opts, err = iterOptionsForBounds(req.Key, req.RangeEnd); err != nil {
			return nil, err
		}
	}
	if !withOlder && through < ^uint64(0) {
		opts.PointKeyFilters = []pebble.BlockPropertyFilter{rp.NewMVCCSeqnoRangeFilter(req.From, through+1)}
	}
	return reader.NewIter(opts)
}
