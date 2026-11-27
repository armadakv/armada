// Copyright Armada Contributors

package fsm

import (
	"sync"
	"testing"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	pb "google.golang.org/protobuf/proto"
)

type recordingObserver struct {
	mu        sync.Mutex
	capturing bool
	commits   []Commit
	closed    bool
}

func (o *recordingObserver) Capturing() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.capturing
}

func (o *recordingObserver) Committed(c Commit) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.commits = append(o.commits, c)
}

func (o *recordingObserver) Closed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closed = true
}

func (o *recordingObserver) last() Commit {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commits[len(o.commits)-1]
}

func observedSM(t *testing.T, fs vfs.FS) (*FSM, *recordingObserver) {
	t.Helper()
	obs := &recordingObserver{capturing: true}
	p := &FSM{
		fs:          fs,
		clusterID:   1,
		nodeID:      1,
		dirname:     "/tmp/watch",
		log:         zap.NewNop().Sugar(),
		metrics:     newMetrics(testTable, 1),
		appliedFunc: func(uint64) {},
		observer:    obs,
	}
	_, err := p.Open(nil)
	require.NoError(t, err)
	return p, obs
}

func applyCommands(t *testing.T, p *FSM, index uint64, cmds ...*armadapb.Command) {
	t.Helper()
	entries := make([]sm.Entry, len(cmds))
	for i, cmd := range cmds {
		entries[i] = sm.Entry{Index: index + uint64(i), Cmd: mustMarshallProto(cmd)}
	}
	_, err := p.Update(entries)
	require.NoError(t, err)
}

func putCmd(k, v string) *armadapb.Command {
	return &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_PUT, Kv: &armadapb.KeyValue{Key: []byte(k), Value: []byte(v)}}
}

func deleteCmd(k string, rangeEnd []byte) *armadapb.Command {
	return &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte(k)}, RangeEnd: rangeEnd}
}

func kv(k, v string, create, mod int64) *armadapb.KeyValue {
	res := &armadapb.KeyValue{Key: []byte(k), CreateRevision: create, ModRevision: mod}
	if v != "" {
		res.Value = []byte(v)
	}
	return res
}

func putEvent(k, v string, create, mod int64, prev *armadapb.KeyValue) *armadapb.Event {
	return &armadapb.Event{Type: armadapb.Event_PUT, Kv: kv(k, v, create, mod), PrevKv: prev}
}

func deleteEvent(k string, create, mod int64, prev *armadapb.KeyValue) *armadapb.Event {
	return &armadapb.Event{Type: armadapb.Event_DELETE, Kv: kv(k, "", create, mod), PrevKv: prev}
}

func requireEvents(t *testing.T, want []*armadapb.Event, got []*armadapb.Event) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.Truef(t, pb.Equal(want[i], got[i]), "event %d: want %v, got %v", i, want[i], got[i])
	}
}

func TestFSM_ObserverOpen(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()
	c := obs.last()
	require.Equal(t, uint64(0), c.Revision)
	require.False(t, c.Captured, "open reports the revision without events")
}

func TestFSM_CapturePutAndDelete(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	applyCommands(t, p, 10, putCmd("a", "1"))
	c := obs.last()
	require.True(t, c.Captured)
	require.Equal(t, uint64(10), c.Revision)
	requireEvents(t, []*armadapb.Event{putEvent("a", "1", 10, 10, nil)}, c.Events)

	applyCommands(t, p, 11, putCmd("a", "2"))
	requireEvents(t, []*armadapb.Event{putEvent("a", "2", 10, 11, kv("a", "1", 10, 10))}, obs.last().Events)

	applyCommands(t, p, 12, deleteCmd("a", nil))
	requireEvents(t, []*armadapb.Event{deleteEvent("a", 10, 12, kv("a", "2", 10, 11))}, obs.last().Events)

	// Deleting a missing key changes nothing and produces no event.
	applyCommands(t, p, 13, deleteCmd("a", nil))
	c = obs.last()
	require.Empty(t, c.Events)
	require.Equal(t, uint64(13), c.Revision)

	// Recreating the key starts a new incarnation without a previous value.
	applyCommands(t, p, 14, putCmd("a", "3"))
	requireEvents(t, []*armadapb.Event{putEvent("a", "3", 14, 14, nil)}, obs.last().Events)
}

func TestFSM_CaptureRangeDelete(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	applyCommands(t, p, 1, putCmd("k1", "v1"), putCmd("k2", "v2"), putCmd("k3", "v3"), putCmd("x", "v"))
	c := obs.last()
	require.Equal(t, uint64(4), c.Revision)
	requireEvents(t, []*armadapb.Event{
		putEvent("k1", "v1", 1, 1, nil),
		putEvent("k2", "v2", 2, 2, nil),
		putEvent("k3", "v3", 3, 3, nil),
		putEvent("x", "v", 4, 4, nil),
	}, c.Events)

	applyCommands(t, p, 5, deleteCmd("k2", nil), deleteCmd("k", []byte("l")))
	requireEvents(t, []*armadapb.Event{
		deleteEvent("k2", 2, 5, kv("k2", "v2", 2, 2)),
		deleteEvent("k1", 1, 6, kv("k1", "v1", 1, 1)),
		deleteEvent("k3", 3, 6, kv("k3", "v3", 3, 3)),
	}, obs.last().Events)
}

func TestFSM_CaptureTxnSameKey(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	applyCommands(t, p, 1, putCmd("a", "0"))
	txn := &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_TXN, Txn: &armadapb.Txn{Success: []*armadapb.RequestOp{
		{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{Key: []byte("a"), Value: []byte("1")}}},
		{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{Key: []byte("a"), Value: []byte("2")}}},
		{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{Key: []byte("b"), Value: []byte("1")}}},
		{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{Key: []byte("b")}}},
	}}}
	applyCommands(t, p, 2, txn)

	// One event per key and revision: the final state, with the state before
	// the revision as the previous key-value.
	requireEvents(t, []*armadapb.Event{
		putEvent("a", "2", 1, 2, kv("a", "0", 1, 1)),
		deleteEvent("b", 2, 2, nil),
	}, obs.last().Events)
}

func TestFSM_NotCapturing(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()
	obs.capturing = false

	applyCommands(t, p, 7, putCmd("a", "1"))
	c := obs.last()
	require.False(t, c.Captured)
	require.Nil(t, c.Events)
	require.Equal(t, uint64(7), c.Revision)
}

func TestFSM_CaptureReplicatedSequence(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	li := func(i uint64) *uint64 { return &i }
	child := func(cmd *armadapb.Command, idx uint64) *armadapb.Command {
		cmd.LeaderIndex = li(idx)
		return cmd
	}
	// Live replication: children carry consecutive source indexes and the
	// outer command the last one. The local Raft index is unrelated.
	seq := &armadapb.Command{
		Table:       []byte(testTable),
		Type:        armadapb.Command_SEQUENCE,
		LeaderIndex: li(102),
		Sequence: []*armadapb.Command{
			child(putCmd("b", "1"), 100),
			child(putCmd("a", "1"), 101),
			child(&armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_DUMMY}, 102),
		},
	}
	applyCommands(t, p, 5, seq)
	c := obs.last()
	require.Equal(t, uint64(102), c.Revision)
	requireEvents(t, []*armadapb.Event{
		putEvent("b", "1", 100, 100, nil),
		putEvent("a", "1", 101, 101, nil),
	}, c.Events)
}

func TestFSM_SnapshotReplayRewrites(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	li := func(i uint64) *uint64 { return &i }
	record := &armadapb.Command{
		Table:       []byte(testTable),
		Type:        armadapb.Command_PUT,
		LeaderIndex: li(40),
		Kv:          &armadapb.KeyValue{Key: []byte("a"), Value: []byte("1"), CreateRevision: 30, ModRevision: 40},
	}
	replay := &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_SEQUENCE, Sequence: []*armadapb.Command{record}}
	applyCommands(t, p, 1, replay)
	c := obs.last()
	require.True(t, c.Rewritten, "records with source revisions rewrite history")
	require.Empty(t, c.Events)
	require.Equal(t, uint64(40), c.Revision)

	terminal := &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_DUMMY, LeaderIndex: li(55)}
	applyCommands(t, p, 2, terminal)
	c = obs.last()
	require.True(t, c.Rewritten, "the terminal marker completes the replay")
	require.Equal(t, uint64(55), c.Revision)
}

func TestFSM_RevisionResetAndGC(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	defer p.Close()

	applyCommands(t, p, 20, putCmd("a", "1"))
	require.Equal(t, uint64(20), obs.last().Revision)

	// GC proposed locally carries the compacted index, not a revision.
	compacted := uint64(3)
	applyCommands(t, p, 21, &armadapb.Command{Type: armadapb.Command_GC, LeaderIndex: &compacted})
	c := obs.last()
	require.Equal(t, uint64(20), c.Revision)
	require.False(t, c.Rewritten)

	zero := uint64(0)
	applyCommands(t, p, 22, &armadapb.Command{Table: []byte(testTable), Type: armadapb.Command_DUMMY, LeaderIndex: &zero})
	c = obs.last()
	require.True(t, c.Rewritten)
	require.Equal(t, uint64(0), c.Revision)
}

func TestFSM_RevisionPersisted(t *testing.T) {
	fs := vfs.NewMem()
	p, _ := observedSM(t, fs)
	applyCommands(t, p, 33, putCmd("a", "1"))
	require.NoError(t, p.Close())

	p, obs := observedSM(t, fs)
	defer p.Close()
	require.Equal(t, uint64(33), obs.last().Revision)
	require.Equal(t, uint64(33), p.revision.Load())
}

func TestFSM_ObserverClosed(t *testing.T) {
	p, obs := observedSM(t, vfs.NewMem())
	require.NoError(t, p.Close())
	require.True(t, obs.closed)
}
