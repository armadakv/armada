// Copyright Armada Contributors

package fsm

import (
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func gcLeaderIndexSM(t *testing.T) (*FSM, *[]uint64) {
	t.Helper()
	var applied []uint64
	p := &FSM{
		fs:          vfs.NewMem(),
		clusterID:   1,
		nodeID:      1,
		dirname:     "/tmp/gc",
		log:         zap.NewNop().Sugar(),
		metrics:     newMetrics(testTable, 1),
		appliedFunc: func(a uint64) { applied = append(applied, a) },
	}
	_, err := p.Open(nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = p.Close() })
	return p, &applied
}

func updateEntries(t *testing.T, p *FSM, index uint64, cmds ...*armadapb.Command) {
	t.Helper()
	entries := make([]sm.Entry, len(cmds))
	for i, cmd := range cmds {
		entries[i] = sm.Entry{Index: index + uint64(i), Cmd: mustMarshallProto(cmd)}
	}
	_, err := p.Update(entries)
	require.NoError(t, err)
}

func leaderIndexOf(t *testing.T, p *FSM) uint64 {
	t.Helper()
	res, err := p.Lookup(LeaderIndexRequest{})
	require.NoError(t, err)
	return res.(*IndexResponse).Index
}

func replicated(leaderIndex uint64) *armadapb.Command {
	li := leaderIndex
	return &armadapb.Command{
		Table:       []byte(testTable),
		Type:        armadapb.Command_SEQUENCE,
		LeaderIndex: &li,
		Sequence: []*armadapb.Command{{
			Table:       []byte(testTable),
			Type:        armadapb.Command_PUT,
			LeaderIndex: &li,
			Kv:          &armadapb.KeyValue{Key: []byte("k"), Value: []byte("v")},
		}},
	}
}

func localGC(compacted uint64) *armadapb.Command {
	return &armadapb.Command{Type: armadapb.Command_GC, LeaderIndex: &compacted}
}

// A follower cluster proposes GC from its own log compaction, with its local
// compacted index. That index must not replace the leader index that
// replication resumes from.
func TestFSM_LocalGCKeepsLeaderIndex(t *testing.T) {
	t.Run("separate batches", func(t *testing.T) {
		p, applied := gcLeaderIndexSM(t)
		updateEntries(t, p, 10, replicated(2000))
		updateEntries(t, p, 11, localGC(9))
		require.Equal(t, uint64(2000), leaderIndexOf(t, p))
		// A batch with only GC reports nothing.
		require.Equal(t, uint64(2000), (*applied)[len(*applied)-1])
	})
	t.Run("same batch", func(t *testing.T) {
		p, applied := gcLeaderIndexSM(t)
		updateEntries(t, p, 10, replicated(2000), localGC(9))
		require.Equal(t, uint64(2000), leaderIndexOf(t, p))
		require.Equal(t, uint64(2000), (*applied)[len(*applied)-1])
	})
	t.Run("horizon still applies", func(t *testing.T) {
		p, _ := gcLeaderIndexSM(t)
		updateEntries(t, p, 10, replicated(2000), localGC(9))
		res, err := p.Lookup(GCHorizonRequest{})
		require.NoError(t, err)
		require.Equal(t, uint64(9), res.(*IndexResponse).Index)
	})
}

// On the leader cluster GC carries the compacted index too. It must not
// create a leader index where there is none, nor be reported as applied.
func TestFSM_LeaderGCDoesNotWriteLeaderIndex(t *testing.T) {
	p, applied := gcLeaderIndexSM(t)
	updateEntries(t, p, 10, putCmdGC("a"))
	updateEntries(t, p, 11, localGC(5))
	require.Equal(t, uint64(0), leaderIndexOf(t, p))
	// The GC-only batch reports nothing; the last report is the put's index.
	require.Equal(t, uint64(10), (*applied)[len(*applied)-1])

	// A reset still writes its zero leader index.
	zero := uint64(0)
	updateEntries(t, p, 12, replicated(300))
	updateEntries(t, p, 13, &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_DUMMY, LeaderIndex: &zero})
	require.Equal(t, uint64(0), leaderIndexOf(t, p))
}

func putCmdGC(k string) *armadapb.Command {
	return &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_PUT, Kv: &armadapb.KeyValue{Key: []byte(k), Value: []byte("v")}}
}

// versionsOf returns the modification sequences of every stored version of a key.
func versionsOf(t *testing.T, p *FSM, userKey string) []uint64 {
	t.Helper()
	iter, err := p.pebble.Load().NewIter(nil)
	require.NoError(t, err)
	defer iter.Close()
	var seqs []uint64
	for valid := iter.First(); valid; valid = iter.Next() {
		k, err := key.DecodeBytes(iter.Key())
		require.NoError(t, err)
		if k.KeyType == key.TypeUser && string(k.Key) == userKey {
			seqs = append(seqs, k.Seqno)
		}
	}
	return seqs
}

func gcHorizonOf(t *testing.T, p *FSM) (memory, stored uint64) {
	t.Helper()
	stored, err := readLocalIndex(p.pebble.Load(), sysGCHorizon)
	require.NoError(t, err)
	return p.gcHorizon.Load(), stored
}

// Replication rewrites the leader index of the GC entry to the entry's own
// index; the leader's horizon travels in gc_horizon. A GC nested in the
// replicated SEQUENCE must advance the in-memory horizon and wake the sweeper,
// not only persist it.
func TestFSM_ReplicatedGCHorizon(t *testing.T) {
	p, _ := gcLeaderIndexSM(t)
	// An old version of "k" that the version at 1000 shadows.
	updateEntries(t, p, 9, replicated(400))
	require.Len(t, versionsOf(t, p, "k"), 1)
	li, horizon := uint64(1001), uint64(500)
	seq := replicated(1000)
	seq.LeaderIndex = &li
	seq.Sequence = append(seq.Sequence, &armadapb.Command{Type: armadapb.Command_GC, LeaderIndex: &li, GcHorizon: &horizon})
	updateEntries(t, p, 10, seq)

	memory, stored := gcHorizonOf(t, p)
	require.Equal(t, uint64(500), memory)
	require.Equal(t, uint64(500), stored)
	require.Equal(t, uint64(1001), leaderIndexOf(t, p))
	// The sweeper runs now, not only after a restart: the shadowed version
	// below the horizon goes away.
	require.Eventually(t, func() bool {
		return len(versionsOf(t, p, "k")) == 1
	}, 5*time.Second, 10*time.Millisecond, "the version below the horizon was not swept")

	// Without gc_horizon, as proposed by an older leader, the entry's
	// sequence is the horizon, as before.
	li2 := uint64(1200)
	old := replicated(1199)
	old.LeaderIndex = &li2
	old.Sequence = append(old.Sequence, &armadapb.Command{Type: armadapb.Command_GC, LeaderIndex: &li2})
	updateEntries(t, p, 11, old)
	memory, _ = gcHorizonOf(t, p)
	require.Equal(t, uint64(1200), memory)
}

// The horizon never moves back: versions below an applied horizon may already
// be swept, and a follower cluster's own GC carries a horizon in its local
// index domain.
func TestFSM_GCHorizonMonotonic(t *testing.T) {
	p, _ := gcLeaderIndexSM(t)
	updateEntries(t, p, 10, localGC(500))
	updateEntries(t, p, 11, localGC(9))
	memory, stored := gcHorizonOf(t, p)
	require.Equal(t, uint64(500), memory)
	require.Equal(t, uint64(500), stored)

	// The horizon survives a restart.
	require.NoError(t, p.Close())
	_, err := p.Open(nil)
	require.NoError(t, err)
	memory, _ = gcHorizonOf(t, p)
	require.Equal(t, uint64(500), memory)
}
