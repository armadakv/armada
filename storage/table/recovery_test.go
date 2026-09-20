// Copyright Armada Contributors

package table

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/vfs"
	pvfs "github.com/cockroachdb/pebble/v2/vfs"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// casStore is an in-memory store with the same compare-and-swap semantics as
// kv.RaftStore: a write must name the current version, and a successful write
// assigns a fresh, monotonically increasing one. kv.MapStore does neither, so
// it cannot exercise the journal's ordering rules.
type casStore struct {
	mu   sync.Mutex
	m    map[string]kv.Pair
	next uint64
}

func newCASStore() *casStore {
	return &casStore{m: make(map[string]kv.Pair)}
}

func (s *casStore) Exists(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.m[key]
	return ok, nil
}

func (s *casStore) Get(key string) (kv.Pair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[key]
	if !ok {
		return kv.Pair{}, kv.ErrNotExist
	}
	return p, nil
}

func (s *casStore) Set(key, value string, ver uint64) (kv.Pair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.m[key]; ok && cur.Ver != ver {
		return cur, kv.ErrVersionMismatch
	}
	s.next++
	p := kv.Pair{Key: key, Value: value, Ver: s.next}
	s.m[key] = p
	return p, nil
}

func (s *casStore) Delete(key string, ver uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[key]
	if !ok {
		return kv.ErrNotExist
	}
	if cur.Ver != ver {
		return kv.ErrVersionMismatch
	}
	delete(s.m, key)
	return nil
}

func (s *casStore) GetAll(pattern string) ([]kv.Pair, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []kv.Pair
	for _, p := range s.m {
		ok, err := path.Match(pattern, p.Key)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func TestRecoveryJournal(t *testing.T) {
	newManager := func() *Manager {
		cfg := minimalTestConfig()
		return &Manager{
			cfg:              cfg,
			store:            newCASStore(),
			log:              zap.NewNop().Sugar(),
			recoveryInflight: make(map[string]struct{}),
		}
	}

	t.Run("begin pins the artifact and is idempotent", func(t *testing.T) {
		m := newManager()
		art := RecoveryArtifact{ObjectKey: "snapshots/t/full/10.snap", Type: ArtifactFull, TipIndex: 10}
		require.NoError(t, m.BeginRecovery("t", 7, art))

		rec, ok, err := m.RecoveryStatus("t")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, PhaseDownloading, rec.Phase)
		require.Equal(t, art, rec.Artifact)
		require.Equal(t, uint64(7), rec.SourceID)
		require.Equal(t, m.cfg.NodeID, rec.Coordinator)

		// Re-negotiating the same artifact must not churn the record.
		require.NoError(t, m.BeginRecovery("t", 7, art))
		again, _, err := m.getRecovery("t")
		require.NoError(t, err)
		require.Equal(t, rec.UpdatedAt, again.UpdatedAt)
	})

	t.Run("a newly offered artifact replaces the pinned one while downloading", func(t *testing.T) {
		m := newManager()
		require.NoError(t, m.BeginRecovery("t", 7, RecoveryArtifact{ObjectKey: "a", Type: ArtifactFull}))
		require.NoError(t, m.BeginRecovery("t", 7, RecoveryArtifact{ObjectKey: "b", Type: ArtifactFull}))
		rec, _, err := m.getRecovery("t")
		require.NoError(t, err)
		require.Equal(t, "b", rec.Artifact.ObjectKey)
	})

	t.Run("the artifact stays pinned once loading has started", func(t *testing.T) {
		m := newManager()
		require.NoError(t, m.BeginRecovery("t", 7, RecoveryArtifact{ObjectKey: "a", Type: ArtifactFull}))
		rec, ver, err := m.getRecovery("t")
		require.NoError(t, err)
		require.NoError(t, m.setPhase(&rec, &ver, PhaseLoading))

		require.NoError(t, m.BeginRecovery("t", 7, RecoveryArtifact{ObjectKey: "b", Type: ArtifactFull}))
		rec, _, err = m.getRecovery("t")
		require.NoError(t, err)
		require.Equal(t, "a", rec.Artifact.ObjectKey, "switching artifacts mid-load would mix two points in time")
	})

	t.Run("another node's recovery is not taken over", func(t *testing.T) {
		m := newManager()
		rec := RecoveryRecord{Table: "t", Phase: PhaseLoading, Coordinator: 42}
		_, err := m.putRecovery(&rec, 0)
		require.NoError(t, err)

		require.ErrorIs(t, m.BeginRecovery("t", 7, RecoveryArtifact{Type: ArtifactFull}), ErrRecoveryConflict)
		require.ErrorIs(t, m.CompleteDownload(t.Context(), "t", "/tmp/x"), ErrRecoveryConflict)
		require.ErrorIs(t, m.DriveRecovery(t.Context(), "t"), ErrRecoveryConflict)
	})

	t.Run("a stale version loses the compare-and-swap", func(t *testing.T) {
		m := newManager()
		rec := RecoveryRecord{Table: "t", Phase: PhaseDownloading, Coordinator: m.cfg.NodeID}
		ver, err := m.putRecovery(&rec, 0)
		require.NoError(t, err)
		_, err = m.putRecovery(&rec, ver)
		require.NoError(t, err)

		// The first writer's version is now stale.
		_, err = m.putRecovery(&rec, ver)
		require.ErrorIs(t, err, kv.ErrVersionMismatch)
	})

	t.Run("progress is reported per node and cleared with the record", func(t *testing.T) {
		m := newManager()
		m.members = map[uint64]string{1: "a", 2: "b"}
		rec := RecoveryRecord{Table: "t", Phase: PhaseSeeding, Coordinator: 1, RecoveryID: 10002}
		ver, err := m.putRecovery(&rec, 0)
		require.NoError(t, err)

		m.cfg.NodeID = 2
		m.reportProgress(rec, 99)
		require.Equal(t, uint64(99), m.progressOf(rec, 2))

		require.NoError(t, m.deleteRecovery(rec, ver))
		require.Equal(t, uint64(0), m.progressOf(rec, 2))
	})

	t.Run("progress from an older recovery generation is ignored", func(t *testing.T) {
		m := newManager()
		m.cfg.NodeID = 2
		old := RecoveryRecord{Table: "t", RecoveryID: 10001, Coordinator: 1}
		current := RecoveryRecord{Table: "t", RecoveryID: 10002, Coordinator: 1}

		m.reportProgress(old, 99)
		m.progress.Store(progressCacheKey(old.Table, old.RecoveryID, m.cfg.NodeID), uint64(100))
		require.Equal(t, uint64(100), m.progressOf(old, m.cfg.NodeID))
		require.Zero(t, m.progressOf(current, m.cfg.NodeID), "progress indexes from different recovery shards are incomparable")
	})

	t.Run("drive result distinguishes absence and download", func(t *testing.T) {
		m := newManager()
		result, err := m.DriveRecoveryResult(context.Background(), "missing")
		require.NoError(t, err)
		require.Equal(t, RecoveryNoRecovery, result)

		require.NoError(t, m.BeginRecovery("t", 7, RecoveryArtifact{Type: ArtifactFull}))
		result, err = m.DriveRecoveryResult(context.Background(), "t")
		require.NoError(t, err)
		require.Equal(t, RecoveryNeedsDownload, result)
	})

	t.Run("stale abandonment fails before cleanup", func(t *testing.T) {
		m := newManager()
		old := RecoveryRecord{Table: "t", Phase: PhaseDownloading, Coordinator: m.cfg.NodeID}
		ver, err := m.putRecovery(&old, 0)
		require.NoError(t, err)

		newer := old
		newer.Artifact.ObjectKey = "new-artifact"
		_, err = m.putRecovery(&newer, ver)
		require.NoError(t, err)

		require.ErrorIs(t, m.abandonRecovery(old, ver), kv.ErrVersionMismatch)
		got, _, err := m.getRecovery("t")
		require.NoError(t, err)
		require.Equal(t, "new-artifact", got.Artifact.ObjectKey)
	})
}

func TestRecoveryRoleMarker(t *testing.T) {
	m := &Manager{cfg: minimalTestConfig(), log: zap.NewNop().Sugar()}

	require.False(t, m.hasNonVotingMarker(10001))
	require.Empty(t, m.listNonVotingMarkers())

	require.NoError(t, m.writeNonVotingMarker(10001))
	require.True(t, m.hasNonVotingMarker(10001))
	require.Equal(t, []uint64{10001}, m.listNonVotingMarkers())
	require.False(t, m.hasNonVotingMarker(10002))

	// A corrupt marker must read as present: starting non-voting when the
	// membership says voter self-corrects, while the reverse panics.
	f, err := m.markerFS().Create(m.roleMarkerPath(10001), "test")
	require.NoError(t, err)
	_, err = f.Write([]byte("garbage"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.True(t, m.hasNonVotingMarker(10001))

	require.NoError(t, m.clearNonVotingMarker(10001))
	require.False(t, m.hasNonVotingMarker(10001))
	// Clearing a marker that is already gone is not an error.
	require.NoError(t, m.clearNonVotingMarker(10001))
}

// TestRecoveryLearnerFirst exercises the whole learner-first path on a real
// three-node Raft cluster: one node loads the snapshot, the other two join the
// recovery shard as non-voting learners, are promoted once caught up, and the
// shard is swapped in as the table's serving shard.
func TestRecoveryLearnerFirst(t *testing.T) {
	const tableName = "learner"
	r := require.New(t)

	cluster := startManagerCluster(t, 3)
	defer cluster.close()

	coordinator := cluster.managers[0]
	_, err := coordinator.CreateTable(tableName)
	r.NoError(err)
	cluster.waitForShard(t, func(m *Manager) uint64 {
		tab, err := m.GetTable(tableName)
		if err != nil {
			return 0
		}
		return tab.ClusterID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tab, err := coordinator.GetTable(tableName)
	r.NoError(err)
	originalShard := tab.ClusterID
	for i := range 8 {
		_, err := tab.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   snapshotTestKey(i),
			Value: []byte(strings.Repeat(string(rune('a'+i)), 128)),
		})
		r.NoError(err)
	}

	sf := buildSnapshotFile(t, ctx, tab, tableName)
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	// Watch the peers while the restore runs. A learner marker is written before
	// a peer joins non-voting, so seeing one is direct evidence the peers were
	// seeded rather than started as voters.
	done := make(chan error, 1)
	go func() { done <- coordinator.Restore(ctx, tableName, sf) }()

	sawLearner := make(map[uint64]bool)
	poll := time.NewTicker(2 * time.Millisecond)
	defer poll.Stop()
	var restoreErr error
watch:
	for {
		select {
		case restoreErr = <-done:
			break watch
		case <-poll.C:
			for _, m := range cluster.managers[1:] {
				if len(m.listNonVotingMarkers()) > 0 {
					sawLearner[m.cfg.NodeID] = true
				}
			}
		case <-ctx.Done():
			t.Fatal("restore did not finish in time")
		}
	}
	r.NoError(restoreErr)
	r.Len(sawLearner, 2, "both peers should have joined the recovery shard as non-voting learners")

	// The table now points at the recovery shard and the journal entry is gone.
	restored, err := coordinator.GetTable(tableName)
	r.NoError(err)
	r.Greater(restored.ClusterID, originalShard, "the recovery shard should have been swapped in")
	_, ok, err := coordinator.RecoveryStatus(tableName)
	r.NoError(err)
	r.False(ok, "the journal record should be deleted once the swap completes")

	// Every member is a voter of the new shard — the peers were added as
	// learners first and promoted only after catching up.
	memCtx, memCancel := context.WithTimeout(ctx, 30*time.Second)
	defer memCancel()
	membership, err := coordinator.nh.SyncGetShardMembership(memCtx, restored.ClusterID)
	r.NoError(err)
	r.Len(membership.Nodes, 3, "all members should be voters after promotion")
	r.Empty(membership.NonVotings, "no learner should be left behind")

	// The data made it, and it is readable through the swapped-in shard.
	rangeResp, err := restored.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	r.NoError(err)
	r.Len(rangeResp.Kvs, 8)

	// The peers carry the shard as voters, while retaining their markers for a
	// future restart. Membership visibility does not prove their AddNonVoting
	// entries have been compacted.
	for _, m := range cluster.managers[1:] {
		r.Eventually(func() bool {
			return m.shardRunning(restored.ClusterID) && m.hasNonVotingMarker(restored.ClusterID)
		}, 30*time.Second, 200*time.Millisecond, "recovered peer should retain its restart-safe marker")
	}

	// The retired shard is tombstoned for the cleanup loop rather than leaked.
	r.Eventually(func() bool {
		p, err := coordinator.store.Get(fmt.Sprintf("/cleanup/%d/%d", coordinator.cfg.NodeID, originalShard))
		return err == nil && p.Value != ""
	}, 30*time.Second, 200*time.Millisecond, "the old shard should be tombstoned for cleanup")
}

// TestRecoveryResumesAfterCoordinatorRestart walks the power-loss matrix: the
// coordinator is stopped at each phase boundary and a fresh Manager on the same
// node picks the journal up and converges without leaking a shard ID.
// TestRecoveryLateMemberJoinsRecoveredShard verifies the serving-shard restart
// path after recovery has already completed. The third member is offline while
// the coordinator seeds and promotes the available learner, then loses all of
// its local NodeHost and table state before returning with the same replica ID.
// It must join the recovered configuration rather than bootstrap the configured
// voter set.
func TestRecoveryLateMemberJoinsRecoveredShard(t *testing.T) {
	const tableName = "late-member"
	r := require.New(t)

	cluster := startManagerCluster(t, 3)
	defer cluster.close()
	for _, m := range cluster.managers {
		m.recoveryInterval = 25 * time.Millisecond
		m.recoveryStepTimeout = 200 * time.Millisecond
		m.recoverySeedTimeout = time.Second
	}

	coordinator := cluster.managers[0]
	_, err := coordinator.CreateTable(tableName)
	r.NoError(err)
	cluster.waitForShard(t, func(m *Manager) uint64 {
		tab, err := m.GetTable(tableName)
		if err != nil {
			return 0
		}
		return tab.ClusterID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tab, err := coordinator.GetTable(tableName)
	r.NoError(err)
	for i := range 4 {
		_, err := tab.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   snapshotTestKey(i),
			Value: []byte(strings.Repeat(string(rune('a'+i)), 64)),
		})
		r.NoError(err)
	}
	sf := buildSnapshotFile(t, ctx, tab, tableName)
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	// Keep this member offline for the entire learner-first recovery, then model
	// replacement of its local disk while preserving its configured identity.
	cluster.stopMember(2)
	cluster.wipeMemberData(2)

	r.NoError(coordinator.Restore(ctx, tableName, sf))
	restored, err := coordinator.GetTable(tableName)
	r.NoError(err)
	r.True(restored.Recovered)
	_, ok, err := coordinator.RecoveryStatus(tableName)
	r.NoError(err)
	r.False(ok, "the journal must be gone before the late member returns")
	lateNode := cluster.reopenMember(t, 2)
	r.False(lateNode.HasNodeInfo(restored.ClusterID, 3),
		"the offline member must have no local recovered-shard state before it rejoins")
	late := cluster.startMember(2, lateNode)
	r.Eventually(func() bool {
		return late.shardRunning(restored.ClusterID)
	}, 30*time.Second, 100*time.Millisecond, "the late member should start the recovered shard by joining")

	// A join starts without conflicting bootstrap membership; the normal serving
	// membership reconciliation then admits the late member as a voter.
	r.Eventually(func() bool {
		membership, err := coordinator.nh.SyncGetShardMembership(ctx, restored.ClusterID)
		return err == nil && len(membership.Nodes) == 3 && len(membership.NonVotings) == 0
	}, 60*time.Second, 200*time.Millisecond, "the late member should join the established recovered membership")

	lateTable, err := late.GetTable(tableName)
	r.NoError(err)
	r.Equal(restored.ClusterID, lateTable.ClusterID)
	r.True(lateTable.Recovered)
	r.Eventually(func() bool {
		resp, err := lateTable.Range(ctx, &armadapb.RangeRequest{
			Key:          []byte{0},
			RangeEnd:     []byte{0},
			Linearizable: true,
		})
		return err == nil && len(resp.Kvs) == 4
	}, 30*time.Second, 100*time.Millisecond, "the late joiner should receive the recovered data")
}

// TestRecoveryResumesAfterNodeHostRestart closes the NodeHost while a recovery
// shard is loaded and reopens it against the same on-disk Raft data. This is a
// real persistence restart, not just a replacement Manager over a live host.
func TestRecoveryResumesAfterNodeHostRestart(t *testing.T) {
	const tableName = "nodehost-restart"
	r := require.New(t)
	root := t.TempDir()
	addr := testRaftAddr(t)
	tableFS := pvfs.NewMem()
	newNode := func() *raft.NodeHost {
		t.Helper()
		nhc := config.NodeHostConfig{
			WALDir:         filepath.Join(root, "wal"),
			NodeHostDir:    filepath.Join(root, "dragonboat"),
			RTTMillisecond: 1,
			RaftAddress:    addr,
		}
		r.NoError(nhc.Prepare())
		nhc.Expert.Engine.ExecShards = 1
		nhc.Expert.LogDB.Shards = 1
		nh, err := raft.NewNodeHost(nhc)
		r.NoError(err)
		return nh
	}
	members := map[uint64]string{1: addr}
	cfg := minimalTestConfig()
	cfg.Table.FS = tableFS
	cfg.Table.DataDir = "tables"
	cfg.Table.MaxInMemLogSize = 1024

	firstNode := newNode()
	store := newCASStore()
	first := NewManager(firstNode, members, store, cfg)
	_, err := first.CreateTable(tableName)
	r.NoError(err)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tab, err := first.GetTable(tableName)
	r.NoError(err)
	for i := range 4 {
		_, err := tab.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   snapshotTestKey(i),
			Value: []byte(strings.Repeat(string(rune('a'+i)), 64)),
		})
		r.NoError(err)
	}
	sf := buildSnapshotFile(t, ctx, tab, tableName)
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()

	recoveryID, err := first.incAndGetIDSeq()
	r.NoError(err)
	r.NoError(first.pinRecoverID(tableName, recoveryID))
	rec := RecoveryRecord{
		Table:       tableName,
		Phase:       PhaseLoading,
		RecoveryID:  recoveryID,
		Coordinator: cfg.NodeID,
		Members:     []uint64{cfg.NodeID},
		Artifact: RecoveryArtifact{
			Type:      ArtifactFull,
			ObjectKey: "snapshots/nodehost-restart/full/1.snap",
			StagePath: sf.Path(),
		},
	}
	ver, err := first.putRecovery(&rec, 0)
	r.NoError(err)
	r.NoError(first.recoveryLoad(ctx, &rec, &ver, nil))
	parked, ok, err := first.RecoveryStatus(tableName)
	r.NoError(err)
	r.True(ok)
	r.Equal(PhaseSeeding, parked.Phase)
	r.True(firstNode.HasNodeInfo(recoveryID, cfg.NodeID), "the loaded recovery shard must be persisted before restart")

	first.Close()
	firstNode.Close()

	reopenedNode := newNode()
	second := NewManager(reopenedNode, members, store, cfg)
	second.recoveryInterval = 25 * time.Millisecond
	second.Start()
	defer func() {
		second.Close()
		reopenedNode.Close()
	}()

	r.Eventually(func() bool {
		_, ok, err := second.RecoveryStatus(tableName)
		return err == nil && !ok
	}, 60*time.Second, 100*time.Millisecond, "reopened NodeHost should resume and complete the persisted recovery")
	recovered, err := second.GetTable(tableName)
	r.NoError(err)
	r.Equal(recoveryID, recovered.ClusterID)
	r.True(recovered.Recovered)
	resp, err := recovered.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	r.NoError(err)
	r.Len(resp.Kvs, 4)
}

// TestBlockedRecoveryLoadDoesNotBlockOtherLearners uses a FIFO-backed staged
// artifact for table A. Keeping the writer open without sending a record leaves
// its snapshot reader blocked in PhaseLoading. Table B is then reconciled
// independently and writes its learner marker, proving that its learner path
// was not held behind A's slow recovery work.
func TestBlockedRecoveryLoadDoesNotBlockOtherLearners(t *testing.T) {
	const (
		slowTable    = "slow"
		learnerTable = "learner"
	)
	r := require.New(t)
	node, members := startRaftNode(t)
	defer node.Close()
	members[2] = testRaftAddr(t)
	m := NewManager(node, members, newCASStore(), minimalTestConfig())
	defer m.Close()
	m.recoveryStepTimeout = time.Second

	slowID, err := m.incAndGetIDSeq()
	r.NoError(err)
	r.NoError(m.pinRecoverID(slowTable, slowID))
	fifo := filepath.Join(t.TempDir(), "blocked-snapshot")
	r.NoError(syscall.Mkfifo(fifo, 0o600))
	defer func() { _ = os.Remove(fifo) }()

	slow := RecoveryRecord{
		Table:       slowTable,
		Phase:       PhaseLoading,
		RecoveryID:  slowID,
		Coordinator: m.cfg.NodeID,
		Members:     []uint64{m.cfg.NodeID},
		Artifact:    RecoveryArtifact{Type: ArtifactFull, ObjectKey: "blocked", StagePath: fifo},
	}
	_, err = m.putRecovery(&slow, 0)
	r.NoError(err)

	writerOpened := make(chan *os.File, 1)
	writerErr := make(chan error, 1)
	go func() {
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			writerErr <- err
			return
		}
		writerOpened <- f
	}()

	r.NoError(m.reconcileRecovery())
	var writer *os.File
	select {
	case writer = <-writerOpened:
	case err := <-writerErr:
		t.Fatal(err)
	case <-time.After(30 * time.Second):
		t.Fatal("slow recovery never opened its staged artifact")
	}
	defer writer.Close()

	r.Eventually(func() bool {
		m.recoveryMu.Lock()
		_, busy := m.recoveryInflight[slowTable]
		m.recoveryMu.Unlock()
		rec, ok, err := m.RecoveryStatus(slowTable)
		return err == nil && ok && busy && rec.Phase == PhaseLoading
	}, time.Second, 10*time.Millisecond, "the FIFO-backed load should remain blocked in the loading phase")

	learnerID, err := m.incAndGetIDSeq()
	r.NoError(err)
	other := RecoveryRecord{
		Table:       learnerTable,
		Phase:       PhaseSeeding,
		RecoveryID:  learnerID,
		Coordinator: 2,
		Members:     []uint64{1, 2},
		Learners:    []uint64{1},
	}
	_, err = m.putRecovery(&other, 0)
	r.NoError(err)

	done := make(chan error, 1)
	go func() { done <- m.reconcileRecovery() }()
	select {
	case err := <-done:
		r.NoError(err)
	case <-time.After(time.Second):
		t.Fatal("a blocked recovery load prevented another table's learner reconciliation")
	}
	r.True(m.hasNonVotingMarker(learnerID), "table B should enter the learner path while table A is still loading")
}

func TestRecoveryResumesAfterCoordinatorRestart(t *testing.T) {
	for _, phase := range []RecoveryPhase{PhaseDownloading, PhaseLoading, PhaseSeeding, PhasePromoting, PhaseSwapping} {
		t.Run(string(phase), func(t *testing.T) {
			const tableName = "resume"
			r := require.New(t)

			node, members := startRaftNode(t)
			defer node.Close()
			store := newCASStore()

			cfg := minimalTestConfig()
			cfg.Table.MaxInMemLogSize = 1024
			first := NewManager(node, members, store, cfg)
			first.recoveryInterval = 50 * time.Millisecond
			first.Start()

			_, err := first.CreateTable(tableName)
			r.NoError(err)

			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			tab, err := first.GetTable(tableName)
			r.NoError(err)
			originalShard := tab.ClusterID
			for i := range 4 {
				_, err := tab.Put(ctx, &armadapb.PutRequest{
					Table: []byte(tableName),
					Key:   snapshotTestKey(i),
					Value: []byte(strings.Repeat(string(rune('a'+i)), 64)),
				})
				r.NoError(err)
			}

			sf := buildSnapshotFile(t, ctx, tab, tableName)
			defer func() {
				_ = sf.Close()
				_ = os.Remove(sf.Path())
			}()

			// Park the recovery at the phase under test, mimicking a crash
			// immediately after the journal write that authorises it.
			recoveryID, err := first.incAndGetIDSeq()
			r.NoError(err)
			rec := RecoveryRecord{
				Table:       tableName,
				Phase:       phase,
				Coordinator: cfg.NodeID,
				RecoveryID:  recoveryID,
				Artifact: RecoveryArtifact{
					Type:      ArtifactFull,
					ObjectKey: "snapshots/resume/full/1.snap",
					StagePath: sf.Path(),
				},
			}
			if phase == PhaseDownloading {
				rec.RecoveryID = 0
				rec.Artifact.StagePath = ""
			}
			if phase == PhaseSeeding || phase == PhasePromoting || phase == PhaseSwapping {
				// Those phases assume the load already happened.
				r.NoError(first.pinRecoverID(tableName, recoveryID))
				r.NoError(first.ensureRecoveryShard(tableName, recoveryID))
				r.NoError(first.waitForLeader(ctx, recoveryID))
				staged, err := snapshot.OpenFile(sf.Path())
				r.NoError(err)
				r.NoError(first.readIntoTable(ctx, recoveryID, tableName, staged, true))
				r.NoError(staged.Close())
			}
			_, err = first.putRecovery(&rec, 0)
			r.NoError(err)
			r.NoError(first.pinRecoverID(tableName, rec.RecoveryID))

			// "Power loss": stop the manager, keep the NodeHost and the
			// replicated store, then bring a new manager up on the same node.
			first.Close()

			second := NewManager(node, members, store, cfg)
			second.recoveryInterval = 50 * time.Millisecond
			second.Start()
			defer second.Close()

			if phase == PhaseDownloading {
				// Nothing local to resume from and no transport here; the
				// record simply stays pinned for the worker to re-drive.
				resumed, ok, err := second.RecoveryStatus(tableName)
				r.NoError(err)
				r.True(ok)
				r.Equal(PhaseDownloading, resumed.Phase)
				return
			}

			r.Eventually(func() bool {
				_, ok, err := second.RecoveryStatus(tableName)
				return err == nil && !ok
			}, 60*time.Second, 200*time.Millisecond, "recovery should converge after restart")

			swapped, err := second.GetTable(tableName)
			r.NoError(err)
			r.Equal(recoveryID, swapped.ClusterID)
			r.Equal(uint64(0), swapped.RecoverID)

			// No shard ID was burned beyond the one the journal named.
			seq, err := store.Get(sequenceKey)
			r.NoError(err)
			r.Equal(fmt.Sprintf("%d", recoveryID), seq.Value,
				"resuming must not allocate a fresh recovery shard id")

			rangeResp, err := swapped.Range(ctx, &armadapb.RangeRequest{
				Key:          []byte{0},
				RangeEnd:     []byte{0},
				Linearizable: true,
			})
			r.NoError(err)
			r.Len(rangeResp.Kvs, 4)
			r.NotEqual(originalShard, swapped.ClusterID)
		})
	}
}

// TestRecoverySeedSkipsExistingVoter checks that the coordinator never proposes
// an AddNonVoting for a replica that is already a voter. Applying such an entry
// on the replica itself is a panic in the Raft layer, not an error.
func TestRecoverySeedSkipsExistingVoter(t *testing.T) {
	const tableName = "voter"
	r := require.New(t)

	cluster := startManagerCluster(t, 2)
	defer cluster.close()

	coordinator := cluster.managers[0]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stand up a shard that already has both members as voters, then ask the
	// seeding step to bring peers in.
	recoveryID, err := coordinator.incAndGetIDSeq()
	r.NoError(err)
	// Claim the shard in table metadata before starting it, in the same order
	// ensureRecoveryShard uses. diffTables stops any running shard above
	// tableIDsRangeStart that no table entry references, so starting first
	// leaves a window in which the reconcile loop tears the shard back down
	// and waitForLeader then blocks for the whole recoveryStepTimeout.
	r.NoError(coordinator.pinRecoverID(tableName, recoveryID))
	for _, m := range cluster.managers {
		r.NoError(m.startTable(tableName, recoveryID))
	}
	r.NoError(coordinator.waitForLeader(ctx, recoveryID))

	rec := RecoveryRecord{
		Table:       tableName,
		Phase:       PhaseSeeding,
		Coordinator: coordinator.cfg.NodeID,
		RecoveryID:  recoveryID,
	}
	ver, err := coordinator.putRecovery(&rec, 0)
	r.NoError(err)

	err = coordinator.recoverySeed(ctx, &rec, &ver)
	// Either it advanced (peer already counts as caught up) or it is waiting —
	// what matters is that it did not try to demote a voter and did not fail.
	if err != nil {
		r.ErrorIs(err, ErrRecoveryPending)
	}

	membership, err := coordinator.nh.SyncGetShardMembership(ctx, recoveryID)
	r.NoError(err)
	r.Len(membership.Nodes, 2)
	r.Empty(membership.NonVotings, "an existing voter must not be re-added as a learner")
}

// TestLeaseHolderStillReportsLearnerProgress covers a deadlock that only shows
// up when the table lease happens to land on a learner rather than on the
// recovery's coordinator, which is why it was intermittent.
//
// A node can be both the table's lease holder and a learner of the recovery
// shard at the same time. reconcileRecovery used to treat those as alternatives
// and take only the lease-holder path, so that node never reported its learner
// progress. The coordinator finishes seeding only once every learner has
// reported, so it sat at "1/2 learners caught up" until the seed timeout while
// the follower served stale data and log replication stayed held off.
func TestLeaseHolderStillReportsLearnerProgress(t *testing.T) {
	const tableName = "leased"
	r := require.New(t)

	node, members := startRaftNode(t)
	defer node.Close()
	store := newCASStore()

	cfg := minimalTestConfig()
	cfg.NodeID = 1
	m := NewManager(node, members, store, cfg)
	m.log = zap.NewNop().Sugar()

	// This node owns the table lease but the record belongs to node 2, and this
	// node is one of its learners.
	r.NoError(m.LeaseTable(tableName, time.Minute))
	r.True(m.holdsTableLease(tableName))

	recoveryID, err := m.incAndGetIDSeq()
	r.NoError(err)
	rec := RecoveryRecord{
		Table:       tableName,
		Phase:       PhaseSeeding,
		Coordinator: 2,
		RecoveryID:  recoveryID,
		Learners:    []uint64{1},
		UpdatedAt:   time.Now().UTC(),
	}
	_, err = m.putRecovery(&rec, 0)
	r.NoError(err)

	r.NoError(m.reconcileRecovery())

	// The learner half must have run. The marker is written before the replica
	// is started, so it is present even though starting a single-node join here
	// cannot succeed — what matters is that the lease did not suppress it.
	r.True(m.hasNonVotingMarker(recoveryID),
		"holding the table lease must not stop a learner from taking part in seeding")
}

// TestStaleLiveRecoveryIsDiscardedOnRestart covers the double-recovery seen in
// the e2e harness:
//
//	serving shard is now 10008 at source index 2061 (artefact live tip 0)
//	serving shard is now 10009 at source index 3464 (artefact live tip 0)
//
// A live snapshot is generated on demand and carries no object key, so its
// staged bytes are only meaningful to the process that fetched them. Resuming
// one after a restart replayed a stale point in time, left the table below the
// leader's GC horizon, and so forced a second full recovery immediately after
// the first — two expensive recoveries where one, or an incremental, would do.
func TestStaleLiveRecoveryIsDiscardedOnRestart(t *testing.T) {
	newManager := func(t *testing.T) *Manager {
		t.Helper()
		node, members := startRaftNode(t)
		t.Cleanup(node.Close)
		cfg := minimalTestConfig()
		cfg.NodeID = 1
		m := NewManager(node, members, newCASStore(), cfg)
		m.log = zap.NewNop().Sugar()
		return m
	}

	// Both records are identical apart from the incarnation, so that is the
	// only thing the outcomes can differ on. PhaseDownloading is the realistic
	// phase here: the replication worker has yet to fetch the artifact, so no
	// staged file is expected and nothing else would reject the record.
	liveRecord := func(t *testing.T, m *Manager, table, incarnation string) {
		t.Helper()
		rec := RecoveryRecord{
			Table:       table,
			Phase:       PhaseDownloading,
			Coordinator: m.cfg.NodeID,
			Artifact:    RecoveryArtifact{Type: ArtifactLive},
			Incarnation: incarnation,
			UpdatedAt:   time.Now().UTC(),
		}
		_, err := m.putRecovery(&rec, 0)
		require.NoError(t, err)
	}

	t.Run("staged by a previous process: discarded", func(t *testing.T) {
		m := newManager(t)
		liveRecord(t, m, "stale", "an-earlier-process")

		require.NoError(t, m.reconcileRecovery())

		require.Eventually(t, func() bool {
			_, ok, err := m.RecoveryStatus("stale")
			return err == nil && !ok
		}, 5*time.Second, 10*time.Millisecond, "a live artifact from a previous process must not be replayed")
	})

	t.Run("staged by this process: kept", func(t *testing.T) {
		m := newManager(t)
		liveRecord(t, m, "fresh", m.incarnation)

		require.NoError(t, m.reconcileRecovery())

		_, ok, err := m.RecoveryStatus("fresh")
		require.NoError(t, err)
		require.True(t, ok, "an in-process live recovery must still be driven to completion")
	})
}

// TestPromotionKeepsQuorum pins the guard that separates the two promotion
// sites, which is the whole reason recoverySeed and reconcilePromotions behave
// differently.
//
// recoverySeed promotes into a shard that starts with a single voter, so
// admitting an uncaught-up second voter makes the quorum 2-of-2 and stalls the
// shard — it therefore refuses to promote a learner that has not reported
// progress. But it also gives up waiting once a majority has reported and swaps
// the shard in with the stragglers still non-voting, and the journal record is
// deleted at that point. reconcilePromotions is what finishes them off, and it
// can do so unconditionally because by then the shard has a majority of
// caught-up voters and one more member does not raise the quorum above them.
//
// Without that second step the table served permanently one voter short, with
// nothing logged as an error — on three nodes that leaves two voters, so losing
// one more costs quorum.
func TestPromotionKeepsQuorum(t *testing.T) {
	// One voter: adding a second makes the quorum 2, which the single existing
	// voter cannot satisfy alone. This is the seeding case and must be refused.
	require.False(t, promotionKeepsQuorum(1))

	// Two voters: quorum stays 2, which they already satisfy.
	require.True(t, promotionKeepsQuorum(2))

	// Three voters: the resulting four members need a quorum of 3, and the
	// three existing voters satisfy that on their own.
	require.True(t, promotionKeepsQuorum(3))

	// Degenerate input must never authorise a membership change.
	require.False(t, promotionKeepsQuorum(0))
	require.False(t, promotionKeepsQuorum(-1))
}

// ─── helpers ─────────────────────────────────────────────────────────────────

type managerCluster struct {
	nodes       []*raft.NodeHost
	managers    []*Manager
	members     map[uint64]string
	nodeConfigs []config.NodeHostConfig
	configs     []Config
	store       store
}

func (c *managerCluster) close() {
	for _, m := range c.managers {
		if m != nil {
			m.Close()
		}
	}
	for _, n := range c.nodes {
		if n != nil {
			n.Close()
		}
	}
}

func (c *managerCluster) stopMember(i int) {
	c.managers[i].Close()
	c.nodes[i].Close()
	c.managers[i] = nil
	c.nodes[i] = nil
}

// wipeMemberData replaces every local filesystem used by member i. The next
// NodeHost has the same replica ID and Raft address, but no bootstrap, WAL, log,
// state-machine, or recovery-role-marker data from its previous incarnation.
func (c *managerCluster) wipeMemberData(i int) {
	c.nodeConfigs[i].Expert.FS = vfs.NewMem()
	c.configs[i].Table.FS = pvfs.NewMem()
}

func (c *managerCluster) reopenMember(t *testing.T, i int) *raft.NodeHost {
	t.Helper()
	nh, err := raft.NewNodeHost(c.nodeConfigs[i])
	require.NoError(t, err)
	c.nodes[i] = nh
	return nh
}

func (c *managerCluster) startMember(i int, nh *raft.NodeHost) *Manager {
	m := NewManager(nh, c.members, c.store, c.configs[i])
	m.reconcileInterval = 200 * time.Millisecond
	m.recoveryInterval = 25 * time.Millisecond
	m.recoverySeedTimeout = time.Second
	m.recoveryStepTimeout = 200 * time.Millisecond
	m.Start()
	c.managers[i] = m
	return m
}

// waitForShard waits until every manager reports a non-zero shard for the table.
func (c *managerCluster) waitForShard(t *testing.T, shardOf func(*Manager) uint64) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, m := range c.managers {
			id := shardOf(m)
			if id == 0 || !m.shardRunning(id) {
				return false
			}
		}
		return true
	}, 60*time.Second, 200*time.Millisecond, "all members should be running the table shard")
}

// startManagerCluster brings up n NodeHosts on loopback plus one Manager each,
// all sharing a single compare-and-swap metadata store — the in-process stand-in
// for the Raft-replicated table store.
func startManagerCluster(t *testing.T, n int) *managerCluster {
	t.Helper()

	members := make(map[uint64]string, n)
	addrs := make([]string, n)
	for i := range n {
		addrs[i] = testRaftAddr(t)
		members[uint64(i+1)] = addrs[i]
	}

	c := &managerCluster{members: members}
	store := newCASStore()
	c.store = store
	for i := range n {
		nhc := config.NodeHostConfig{
			WALDir:         "wal",
			NodeHostDir:    "dragonboat",
			RTTMillisecond: 5,
			RaftAddress:    addrs[i],
		}
		require.NoError(t, nhc.Prepare())
		nhc.Expert.FS = vfs.NewMem()
		nhc.Expert.Engine.ExecShards = 1
		nhc.Expert.LogDB.Shards = 1
		c.nodeConfigs = append(c.nodeConfigs, nhc)
		nh, err := raft.NewNodeHost(nhc)
		require.NoError(t, err)
		c.nodes = append(c.nodes, nh)

		cfg := minimalTestConfig()
		cfg.NodeID = uint64(i + 1)
		cfg.Table.MaxInMemLogSize = 4096
		cfg.Table.SnapshotEntries = 20
		cfg.Table.CompactionOverhead = 5
		c.configs = append(c.configs, cfg)
		m := NewManager(nh, members, store, cfg)
		m.reconcileInterval = 200 * time.Millisecond
		m.recoveryInterval = 100 * time.Millisecond
		m.recoverySeedTimeout = 30 * time.Second
		m.Start()
		c.managers = append(c.managers, m)
	}
	return c
}

// buildSnapshotFile writes a full snapshot of tab, terminated by the DUMMY
// marker that commits source progress, and rewinds it for reading.
// stagedSnapshot is the slice of replication/snapshot's unexported file type
// that these tests need.
type stagedSnapshot interface {
	io.Reader
	Path() string
	Close() error
}

func buildSnapshotFile(t *testing.T, ctx context.Context, tab ActiveTable, tableName string) stagedSnapshot {
	t.Helper()
	sf, err := snapshot.NewTemp()
	require.NoError(t, err)
	resp, err := tab.Snapshot(ctx, sf)
	require.NoError(t, err)
	final, err := (&armadapb.Command{
		Table:       []byte(tableName),
		Type:        armadapb.Command_DUMMY,
		LeaderIndex: &resp.Index,
	}).MarshalVT()
	require.NoError(t, err)
	_, err = sf.Write(final)
	require.NoError(t, err)
	require.NoError(t, sf.Sync())
	_, err = sf.Seek(0, io.SeekStart)
	require.NoError(t, err)
	return sf
}

// TestLearnerRestartAfterPromotion covers the restart hazard the design turns
// on: a replica must choose IsNonVoting before it can read its own persisted
// membership, and the two error directions are not symmetric. Starting
// non-voting over a membership that already says "voter" must recover on its
// own; starting as a voter while an AddNonVoting entry naming this replica is
// still replayable panics the Raft layer instead of returning an error.
//
// Promotion does not prove the AddNonVoting entry was compacted, so the marker
// intentionally persists for the lifetime of the local replica data.
func TestLearnerRestartAfterPromotion(t *testing.T) {
	const tableName = "promoted"
	r := require.New(t)

	cluster := startManagerCluster(t, 2)
	defer cluster.close()

	coordinator := cluster.managers[0]
	peer := cluster.managers[1]

	_, err := coordinator.CreateTable(tableName)
	r.NoError(err)
	cluster.waitForShard(t, func(m *Manager) uint64 {
		tab, err := m.GetTable(tableName)
		if err != nil {
			return 0
		}
		return tab.ClusterID
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	tab, err := coordinator.GetTable(tableName)
	r.NoError(err)
	for i := range 4 {
		_, err := tab.Put(ctx, &armadapb.PutRequest{
			Table: []byte(tableName),
			Key:   snapshotTestKey(i),
			Value: []byte(strings.Repeat(string(rune('a'+i)), 64)),
		})
		r.NoError(err)
	}

	sf := buildSnapshotFile(t, ctx, tab, tableName)
	defer func() {
		_ = sf.Close()
		_ = os.Remove(sf.Path())
	}()
	r.NoError(coordinator.Restore(ctx, tableName, sf))

	recovered, err := coordinator.GetTable(tableName)
	r.NoError(err)
	shardID := recovered.ClusterID

	// The learner was promoted, but its role marker deliberately remains: local
	// membership cannot prove that the AddNonVoting log entry is compacted.
	r.Eventually(func() bool {
		membership, err := coordinator.nh.SyncGetShardMembership(ctx, shardID)
		if err != nil {
			return false
		}
		_, voter := membership.Nodes[peer.cfg.NodeID]
		return peer.shardRunning(shardID) && voter && peer.hasNonVotingMarker(shardID)
	}, 60*time.Second, 100*time.Millisecond, "peer should be promoted while retaining its restart-safe marker")

	// Restart after promotion. The persisted marker forces the safe role before
	// Dragonboat replays its local log; it must then converge as a voter again.
	r.NoError(peer.nh.StopShard(shardID))
	r.Eventually(func() bool {
		return !peer.shardRunning(shardID)
	}, 30*time.Second, 50*time.Millisecond)
	if err := peer.startTable(tableName, shardID); err != nil {
		r.ErrorIs(err, raft.ErrShardAlreadyExist)
	}
	r.Eventually(func() bool {
		return peer.shardRunning(shardID) && peer.hasNonVotingMarker(shardID)
	}, 60*time.Second, 100*time.Millisecond, "a restarted promoted learner must retain its restart-safe marker")

	membership, err := coordinator.nh.SyncGetShardMembership(ctx, shardID)
	r.NoError(err)
	r.Len(membership.Nodes, 2)
	r.Empty(membership.NonVotings)

	rangeResp, err := recovered.Range(ctx, &armadapb.RangeRequest{
		Key:          []byte{0},
		RangeEnd:     []byte{0},
		Linearizable: true,
	})
	r.NoError(err)
	r.Len(rangeResp.Kvs, 4)
}
