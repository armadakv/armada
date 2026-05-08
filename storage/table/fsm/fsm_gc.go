// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"encoding/binary"
	stderrors "errors"
	"time"

	rp "github.com/armadakv/armada/pebble"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
)

const (
	// maxBatchSize maximum size of inmemory batch before commit.
	maxBatchSize    = 16 * 1024 * 1024
	gcWorkerPeriod  = 10 * time.Minute
	gcThrottleSleep = 5 * time.Millisecond // pause between batch commits
	gcThrottleBatch = 512                  // keys processed per batch before sleeping
)

// gcPrefixUpper returns the exclusive upper bound for all physical keys sharing
// the same V2 user-key prefix. The V2 prefix ends with a \x00 separator byte;
// replacing it with \x01 produces a key that sorts strictly after every
// seqno-suffixed version of that user key and strictly before the first
// physical key of the next user key. This is safe because user keys cannot
// contain \x00 bytes (validated at the gRPC layer), so no user key of the form
// userKey+"\x00"+... can exist to confuse the boundary.
func gcPrefixUpper(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)
	upper[len(upper)-1] = 0x01 // replace \x00 separator with \x01
	return upper
}

// incrementStoredSeqno returns a new physical key identical to physKey except
// that the 8-byte big-endian stored seqno in the tail is incremented by one.
// Because stored seqno = ^seqno, incrementing it corresponds to decrementing
// the logical seqno by one — the resulting key is the physical key that would
// immediately follow physKey within the same prefix group.
func incrementStoredSeqno(physKey []byte) []byte {
	out := make([]byte, len(physKey))
	copy(out, physKey)
	seqStart := len(out) - key.V2SeqLen
	stored := binary.BigEndian.Uint64(out[seqStart:])
	binary.BigEndian.PutUint64(out[seqStart:], stored+1)
	return out
}

// runGC performs an explicit MVCC garbage collection sweep up to the given
// raft index. It iterates every physical key in the user keyspace and issues
// DeleteRange tombstones to remove:
//   - all versions with seqno < gcIndex that are shadowed by a newer version
//     of the same logical user key (same key prefix), and
//   - tombstone versions with seqno < gcIndex that have no live version above
//     gcIndex (the key was deleted before the GC horizon).
//
// For each user-key prefix the sweep encounters the first old version (seqno <
// gcIndex), determines the range start, and issues a single DeleteRange from
// that start up to gcPrefixUpper(prefix).  It then calls iter.SeekGE to jump
// directly to the next prefix, skipping all remaining old versions without
// visiting them individually.  This reduces both iterator steps and batch
// entries from O(old-versions) to O(distinct-user-keys).
//
// Three cases per prefix:
//  1. A live version at seqno >= gcIndex exists (hasLiveAbove): delete the
//     first old version and everything older → DeleteRange(firstOld, upper).
//  2. No version >= gcIndex and the newest old version is a tombstone: the key
//     was deleted before the horizon; delete tombstone and everything older →
//     DeleteRange(firstOld, upper).
//  3. No version >= gcIndex and the newest old version is a live value: this is
//     the current live version of the key — keep it; delete any older versions
//     → DeleteRange(firstOld+1, upper).  A no-op when there are no older ones.
//
// Deletions are batched; when the batch reaches maxBatchSize it is committed
// and a new one is opened. After all obsolete versions are removed, pebble's
// Compact is called over the full user keyspace so the deleted entries are
// physically reclaimed from disk.
func (p *FSM) runGC(db *pebble.DB, gcIndex uint64) error {
	if gcIndex == 0 {
		return nil
	}

	// Scan the entire user keyspace using a snapshot so the view is stable
	// for the duration of the sweep.
	snap := db.NewSnapshot()
	defer snap.Close()

	// The MVCC seqno block property filter lets pebble skip entire SST blocks
	// whose recorded seqno interval does not intersect [0, gcIndex). Blocks
	// where every key has seqno >= gcIndex contain nothing the GC can touch and
	// are skipped without even being opened, dramatically reducing I/O for
	// databases with mostly-recent data.
	seqnoFilter := rp.NewMVCCSeqnoFilter(gcIndex)
	iter, err := snap.NewIter(&pebble.IterOptions{
		LowerBound:      mustEncodeKey(key.Key{KeyType: key.TypeUser, Key: key.LatestMinKey}),
		UpperBound:      incrementRightmostByte(append([]byte(nil), maxUserKey...)),
		PointKeyFilters: []pebble.BlockPropertyFilter{seqnoFilter},
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	batch := db.NewBatch(pebble.WithInitialSizeBytes(maxBatchSize))

	var (
		lastPrefix   []byte
		hasLiveAbove bool
	)

	commitBatch := func() error {
		if batch.Count() == 0 {
			return nil
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return err
		}
		batch = db.NewBatch(pebble.WithInitialSizeBytes(maxBatchSize))
		return nil
	}

	// Use a manual advance loop so we can call iter.SeekGE to jump past all
	// old versions of a prefix in a single step instead of visiting each one.
	for valid := iter.First(); valid; {
		physKey := iter.Key()

		// Decode the logical key to get keyType and seqno.
		k, err := key.DecodeBytes(physKey)
		if err != nil {
			return err
		}
		// Only GC user keys; system keys have no MVCC versions.
		if k.KeyType != key.TypeUser {
			valid = iter.Next()
			continue
		}

		// Prefix = everything except the trailing seqno bytes.
		prefixLen := len(physKey) - key.V2SeqLen
		prefix := physKey[:prefixLen]

		if !bytes.Equal(lastPrefix, prefix) {
			lastPrefix = append(lastPrefix[:0], prefix...)
			hasLiveAbove = false
		}

		if k.Seqno >= gcIndex {
			// This version is at or above the safe horizon — always keep it.
			hasLiveAbove = true
			valid = iter.Next()
			continue
		}

		// First old version of this prefix (SeekGE below will skip any
		// further old versions so this branch executes at most once per prefix).
		prefixUpper := gcPrefixUpper(prefix)

		var rangeLo []byte
		if hasLiveAbove || isTombstone(iter.Value()) {
			// Cases 1 & 2: delete the first old key and everything older.
			rangeLo = make([]byte, len(physKey))
			copy(rangeLo, physKey)
		} else {
			// Case 3: keep the current (live) version; delete any older ones.
			rangeLo = incrementStoredSeqno(physKey)
		}

		if err := batch.DeleteRange(rangeLo, prefixUpper, nil); err != nil {
			return err
		}

		if uint64(batch.Len()) >= maxBatchSize {
			if err := commitBatch(); err != nil {
				return err
			}
			time.Sleep(gcThrottleSleep)
		}

		// Jump directly to the next prefix, skipping all remaining old versions.
		valid = iter.SeekGE(prefixUpper)
	}

	if err := commitBatch(); err != nil {
		return err
	}

	return nil
}

// signalGCWorker sends a non-blocking signal to the GC worker to start a sweep.
func (p *FSM) signalGCWorker() {
	select {
	case p.gcCh <- struct{}{}:
	default:
	}
}

// startGCWorker launches the background goroutine that performs throttled
// MVCC GC sweeps. It is started by Open() and stopped by Close().
func (p *FSM) startGCWorker() {
	go func() {
		defer close(p.gcDone)
		ticker := time.NewTicker(gcWorkerPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-p.gcStop:
				return
			case <-p.gcCh:
				p.runGCSweep()
			case <-ticker.C:
				p.runGCSweep()
			}
		}
	}()
}

// runGCSweep runs a single throttled MVCC GC sweep using the current gcHorizon.
func (p *FSM) runGCSweep() {
	horizon := p.gcHorizon.Load()
	if horizon == 0 {
		return
	}
	db := p.pebble.Load()
	if db == nil {
		return
	}
	if err := p.runGC(db, horizon); err != nil {
		if !stderrors.Is(err, pebble.ErrClosed) {
			p.log.Warnf("GC sweep at horizon %d failed: %v", horizon, err)
		}
	}
}
