// Copyright JAMF Software, LLC

package fsm

import (
	"math"
	"testing"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
)

// physicalVersionCount returns the number of raw Pebble entries that exist for
// the given user key across all MVCC versions (including tombstones, including
// versions that are logically invisible due to shadowing).
func physicalVersionCount(db *pebble.DB, userKey []byte) int {
	// Build the prefix: [4B header][TypeUser][userKey][0x00 sep]
	ref := mustEncodeKey(key.Key{KeyType: key.TypeUser, Key: userKey})
	prefixLen := len(ref) - key.V2SeqLen
	prefix := ref[:prefixLen]

	// Lower bound: prefix + 8 zero bytes → stored seqno = 0 → seqno = MaxUint64
	// (sorts FIRST within the prefix group, i.e. newest version).
	lo := make([]byte, len(prefix)+key.V2SeqLen)
	copy(lo, prefix)

	// Upper bound: replace trailing 0x00 separator with 0x01.
	hi := gcPrefixUpper(prefix)

	snap := db.NewSnapshot()
	defer snap.Close()

	iter, err := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		panic(err)
	}
	defer iter.Close()

	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		n++
	}
	return n
}

// lookupVisible returns the current visible value for userKey, or nil if the
// key is absent (including when it has been tombstoned).
func lookupVisible(t *testing.T, p *FSM, userKey []byte) []byte {
	t.Helper()
	res, err := p.Lookup(&armadapb.RequestOp_Range{Key: userKey})
	require.NoError(t, err)
	resp := res.(*armadapb.ResponseOp_Range)
	if len(resp.Kvs) == 0 {
		return nil
	}
	return resp.Kvs[0].Value
}

// putEntry builds an sm.Entry that writes key→value at the given raft index
// (which becomes the MVCC seqno when no LeaderIndex is set).
func putEntry(idx uint64, userKey, value []byte) sm.Entry {
	return sm.Entry{
		Index: idx,
		Cmd: mustMarshallProto(&armadapb.Command{
			Type: armadapb.Command_PUT,
			Kv:   &armadapb.KeyValue{Key: userKey, Value: value},
		}),
	}
}

// deleteEntry builds an sm.Entry that writes a tombstone for key at the given
// raft index.
func deleteEntry(idx uint64, userKey []byte) sm.Entry {
	return sm.Entry{
		Index: idx,
		Cmd: mustMarshallProto(&armadapb.Command{
			Type: armadapb.Command_DELETE,
			Kv:   &armadapb.KeyValue{Key: userKey},
		}),
	}
}

// applyEntries drives entries through the FSM and panics on error.
func applyEntries(p *FSM, entries ...sm.Entry) {
	if _, err := p.Update(entries); err != nil {
		panic(err)
	}
}

func TestRunGC_ZeroIndex(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p, putEntry(1, []byte("k"), []byte("v")))

	require.NoError(t, p.runGC(p.pebble.Load(), 0))
	// gcIndex 0 is a no-op; key must still be visible.
	require.Equal(t, []byte("v"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

func TestRunGC_EmptyDatabase(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	require.NoError(t, p.runGC(p.pebble.Load(), 100))
}

// Case 3: single live version below gcIndex, no version above.
// The key must be kept (it is the current version of the key).
func TestRunGC_SingleLiveVersionBelowHorizon_Kept(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p, putEntry(3, []byte("k"), []byte("v3")))

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	require.Equal(t, []byte("v3"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Case 2: single tombstone version below gcIndex, no version above.
// The tombstone must be deleted (the key was deleted before the GC horizon).
func TestRunGC_SingleTombstoneBelowHorizon_Deleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		deleteEntry(3, []byte("k")),
	)
	require.Equal(t, 2, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	require.Nil(t, lookupVisible(t, p, []byte("k")))
	require.Equal(t, 0, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Case 3 with older versions: multiple live versions all below gcIndex.
// Newest must be kept; all older versions must be removed.
func TestRunGC_MultipleLiveVersionsBelowHorizon_KeepNewestDeleteRest(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(2, []byte("k"), []byte("v2")),
		putEntry(4, []byte("k"), []byte("v4")),
	)
	require.Equal(t, 3, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	require.Equal(t, []byte("v4"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Tombstone is newest below gcIndex and there are older live versions below it.
// All versions (tombstone + older) must be removed since the key was deleted.
func TestRunGC_TombstoneNewest_OlderVersionsBelowHorizon_AllDeleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(2, []byte("k"), []byte("v2")),
		deleteEntry(4, []byte("k")),
	)
	require.Equal(t, 3, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	require.Nil(t, lookupVisible(t, p, []byte("k")))
	require.Equal(t, 0, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Case 1: live version at seqno >= gcIndex plus old versions below.
// Old versions must be deleted; live version must be kept.
func TestRunGC_LiveVersionAboveHorizon_OldVersionsDeleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(3, []byte("k"), []byte("v3")),
		putEntry(7, []byte("k"), []byte("v7")),
	)
	require.Equal(t, 3, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 5))

	require.Equal(t, []byte("v7"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Multiple live versions above gcIndex plus several old versions below.
// All old versions must be removed; all versions at/above horizon kept.
func TestRunGC_MultipleLiveVersionsAboveHorizon_OldVersionsDeleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(2, []byte("k"), []byte("v2")),
		putEntry(3, []byte("k"), []byte("v3")),
		putEntry(7, []byte("k"), []byte("v7")),
		putEntry(9, []byte("k"), []byte("v9")),
	)
	require.Equal(t, 5, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 5))

	require.Equal(t, []byte("v9"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 2, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Case 1 with tombstone: live version above gcIndex, tombstone below gcIndex.
// Tombstone must be deleted; live version kept.
func TestRunGC_TombstoneBelowHorizon_LiveVersionAbove_TombstoneDeleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		deleteEntry(3, []byte("k")),
		putEntry(7, []byte("k"), []byte("v7")),
	)
	require.Equal(t, 3, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), 5))

	require.Equal(t, []byte("v7"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// All versions are at or above gcIndex → nothing should be removed.
func TestRunGC_AllVersionsAboveHorizon_NothingDeleted(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(5, []byte("k"), []byte("v5")),
		putEntry(7, []byte("k"), []byte("v7")),
	)

	require.NoError(t, p.runGC(p.pebble.Load(), 5))

	require.Equal(t, []byte("v7"), lookupVisible(t, p, []byte("k")))
	require.Equal(t, 2, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// gcIndex exactly at a version boundary: seqno == gcIndex must be kept.
func TestRunGC_ExactBoundary_SeqnoAtGCIndex_Kept(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(5, []byte("k"), []byte("v5")), // seqno == gcIndex
	)

	require.NoError(t, p.runGC(p.pebble.Load(), 5))

	require.Equal(t, []byte("v5"), lookupVisible(t, p, []byte("k")))
	// seqno=5 >= gcIndex=5 (kept), seqno=1 < gcIndex=5 (deleted)
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("k")))
}

// Multiple keys — each processed independently and correctly.
func TestRunGC_MultipleKeys_IndependentProcessing(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	const gcIdx = uint64(5)

	// key "a": live above horizon + old below → old deleted
	applyEntries(p,
		putEntry(1, []byte("a"), []byte("a1")),
		putEntry(7, []byte("a"), []byte("a7")),
	)
	// key "b": tombstone only below horizon → deleted
	applyEntries(p,
		putEntry(2, []byte("b"), []byte("b2")),
		deleteEntry(3, []byte("b")),
	)
	// key "c": single live below horizon → kept
	applyEntries(p,
		putEntry(4, []byte("c"), []byte("c4")),
	)
	// key "d": multiple versions above horizon → all kept
	applyEntries(p,
		putEntry(6, []byte("d"), []byte("d6")),
		putEntry(8, []byte("d"), []byte("d8")),
	)

	require.NoError(t, p.runGC(p.pebble.Load(), gcIdx))

	db := p.pebble.Load()

	require.Equal(t, []byte("a7"), lookupVisible(t, p, []byte("a")))
	require.Equal(t, 1, physicalVersionCount(db, []byte("a")))

	require.Nil(t, lookupVisible(t, p, []byte("b")))
	require.Equal(t, 0, physicalVersionCount(db, []byte("b")))

	require.Equal(t, []byte("c4"), lookupVisible(t, p, []byte("c")))
	require.Equal(t, 1, physicalVersionCount(db, []byte("c")))

	require.Equal(t, []byte("d8"), lookupVisible(t, p, []byte("d")))
	require.Equal(t, 2, physicalVersionCount(db, []byte("d")))
}

// Prefix-boundary safety: "foo" and "foobar" are distinct user keys.
// GC on "foo" must not affect "foobar" versions.
func TestRunGC_PrefixBoundarySafety(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		// "foo": 3 versions, all below gcIndex → keep only newest
		putEntry(1, []byte("foo"), []byte("foo1")),
		putEntry(2, []byte("foo"), []byte("foo2")),
		putEntry(3, []byte("foo"), []byte("foo3")),
		// "foobar": 2 versions, all below gcIndex → keep only newest
		putEntry(1, []byte("foobar"), []byte("foobar1")),
		putEntry(4, []byte("foobar"), []byte("foobar4")),
	)

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	db := p.pebble.Load()
	require.Equal(t, []byte("foo3"), lookupVisible(t, p, []byte("foo")))
	require.Equal(t, 1, physicalVersionCount(db, []byte("foo")))

	require.Equal(t, []byte("foobar4"), lookupVisible(t, p, []byte("foobar")))
	require.Equal(t, 1, physicalVersionCount(db, []byte("foobar")))
}

// Keys containing 0x01 bytes must not be confused with the gcPrefixUpper boundary.
func TestRunGC_KeyContainingOxO1Bytes(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	key1 := []byte("foo")
	key2 := []byte{'f', 'o', 'o', 0x01, 'b', 'a', 'r'} // "foo\x01bar"

	applyEntries(p,
		putEntry(1, key1, []byte("foo-v1")),
		putEntry(3, key1, []byte("foo-v3")),
		putEntry(2, key2, []byte("foobar01-v2")),
		putEntry(4, key2, []byte("foobar01-v4")),
	)

	require.NoError(t, p.runGC(p.pebble.Load(), 10))

	db := p.pebble.Load()
	require.Equal(t, []byte("foo-v3"), lookupVisible(t, p, key1))
	require.Equal(t, 1, physicalVersionCount(db, key1))

	require.Equal(t, []byte("foobar01-v4"), lookupVisible(t, p, key2))
	require.Equal(t, 1, physicalVersionCount(db, key2))
}

// Large number of versions ensures the SeekGE-based loop is exercised under
// realistic conditions and produces a correct result.
func TestRunGC_ManyVersions(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	const (
		totalVersions = 100
		gcIdx         = uint64(80)
	)

	entries := make([]sm.Entry, totalVersions)
	for i := range entries {
		entries[i] = putEntry(uint64(i+1), []byte("k"), []byte{byte(i + 1)})
	}
	applyEntries(p, entries...)
	require.Equal(t, totalVersions, physicalVersionCount(p.pebble.Load(), []byte("k")))

	require.NoError(t, p.runGC(p.pebble.Load(), gcIdx))

	// seqno 80..100 survive (>= gcIdx), seqno 1..79 are deleted.
	// (seqno 80 is the first kept: 80 >= gcIdx=80)
	require.Equal(t, totalVersions-int(gcIdx)+1, physicalVersionCount(p.pebble.Load(), []byte("k")))
	require.Equal(t, []byte{byte(totalVersions)}, lookupVisible(t, p, []byte("k")))
}

// Idempotency: running GC twice with the same horizon must not change results.
func TestRunGC_Idempotent(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(3, []byte("k"), []byte("v3")),
		putEntry(7, []byte("k"), []byte("v7")),
	)

	db := p.pebble.Load()
	require.NoError(t, p.runGC(db, 5))
	require.Equal(t, 1, physicalVersionCount(db, []byte("k")))

	require.NoError(t, p.runGC(db, 5))
	require.Equal(t, 1, physicalVersionCount(db, []byte("k")))
	require.Equal(t, []byte("v7"), lookupVisible(t, p, []byte("k")))
}

// Increasing horizon: subsequent GC sweeps with higher gcIndex correctly
// clean up versions that were previously kept.
func TestRunGC_IncreasingHorizon(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	applyEntries(p,
		putEntry(1, []byte("k"), []byte("v1")),
		putEntry(3, []byte("k"), []byte("v3")),
		putEntry(7, []byte("k"), []byte("v7")),
		putEntry(9, []byte("k"), []byte("v9")),
	)

	db := p.pebble.Load()

	// First sweep: gcIndex=5 → seqno 1,3 deleted; seqno 7,9 kept.
	require.NoError(t, p.runGC(db, 5))
	require.Equal(t, 2, physicalVersionCount(db, []byte("k")))
	require.Equal(t, []byte("v9"), lookupVisible(t, p, []byte("k")))

	// Second sweep: gcIndex=8 → seqno 7 now below horizon; only seqno 9 survives.
	require.NoError(t, p.runGC(db, 8))
	require.Equal(t, 1, physicalVersionCount(db, []byte("k")))
	require.Equal(t, []byte("v9"), lookupVisible(t, p, []byte("k")))
}

// Ensure the helper gcPrefixUpper correctly replaces the trailing separator.
func TestGCPrefixUpper(t *testing.T) {
	r := require.New(t)
	ref := mustEncodeKey(key.Key{KeyType: key.TypeUser, Key: []byte("foo")})
	prefixLen := len(ref) - key.V2SeqLen
	prefix := ref[:prefixLen]

	upper := gcPrefixUpper(prefix)
	r.Len(upper, prefixLen)
	r.Equal(byte(0x01), upper[len(upper)-1])

	// upper must sort strictly after any key that starts with prefix+0x00.
	// (i.e. all physical versions of "foo")
	r.Less(string(ref), string(upper)) // ref has 0xFF...FF seqno, upper has 0x01 after prefix
}

// Ensure incrementStoredSeqno produces a key that sorts immediately after
// the input within the same prefix.
func TestIncrementStoredSeqno(t *testing.T) {
	r := require.New(t)

	// Encode key "k" at seqno=42.
	ref := mustEncodeKey(key.Key{KeyType: key.TypeUser, Key: []byte("k"), Seqno: 42})
	next := incrementStoredSeqno(ref)

	// next must be strictly greater than ref and still share the same prefix.
	r.Greater(string(next), string(ref))

	prefixLen := len(ref) - key.V2SeqLen
	r.Equal(ref[:prefixLen], next[:prefixLen])

	// The stored seqno in next must equal stored(42)+1.
	// stored(42) = ^42; next stored = ^42 + 1.
	storedRef := uint64(0)
	for i := prefixLen; i < len(ref); i++ {
		storedRef = storedRef<<8 | uint64(ref[i])
	}
	storedNext := uint64(0)
	for i := prefixLen; i < len(next); i++ {
		storedNext = storedNext<<8 | uint64(next[i])
	}
	r.Equal(storedRef+1, storedNext)
}

// Sanity-check that physicalVersionCount correctly counts entries including
// those with seqno = math.MaxUint64 (which maps to stored seqno = 0).
func TestPhysicalVersionCount_IncludesMaxSeqno(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	// Seqno math.MaxUint64 is the largest possible; stored = 0 (sorts first).
	// We use a raw Pebble write to inject this extreme case.
	db := p.pebble.Load()

	b := db.NewBatch()
	buf := bufferPool.Get()
	require.NoError(t, encodeUserKey(buf, []byte("extreme"), math.MaxUint64))
	require.NoError(t, b.Set(buf.Bytes(), []byte("vmax"), nil))
	bufferPool.Put(buf)
	require.NoError(t, b.Commit(pebble.NoSync))

	require.Equal(t, 1, physicalVersionCount(db, []byte("extreme")))
}
