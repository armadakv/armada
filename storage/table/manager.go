// Copyright JAMF Software, LLC

// Package table manages the lifecycle of Armada tables.
// Each table is backed by its own Raft consensus group and a Pebble-based state machine.
// The Manager is responsible for starting, stopping, and coordinating per-table Raft replicas.
package table

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/armadakv/armada/armadapb"
	ap "github.com/armadakv/armada/pebble"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/config"
	sm "github.com/armadakv/armada/raft/statemachine"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/cenkalti/backoff/v4"
	"github.com/cockroachdb/pebble/v2"
	"go.uber.org/zap"
)

type store interface {
	Exists(key string) (bool, error)
	Set(key string, value string, ver uint64) (kv.Pair, error)
	Delete(key string, ver uint64) error
	Get(key string) (kv.Pair, error)
	GetAll(pattern string) ([]kv.Pair, error)
}

type leaderStore interface {
	HasLeader() bool
}

const (
	keyPrefix                       = "/tables/"
	sequenceKey                     = keyPrefix + "sys/idseq"
	tableIDsRangeStart       uint64 = 10000
	initialReconcileInterval        = 100 * time.Millisecond
)

func NewManager(nh *raft.NodeHost, members map[uint64]string, store store, cfg Config) *Manager {
	blockCache := pebble.NewCache(cfg.Table.BlockCacheSize)
	scheduler := ap.NewConcurrencyLimitScheduler()
	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	return &Manager{
		lifeCtx:             lifeCtx,
		lifeCancel:          lifeCancel,
		nh:                  nh,
		reconcileInterval:   30 * time.Second,
		cleanupInterval:     30 * time.Second,
		cleanupGracePeriod:  5 * time.Minute,
		cleanupTimeout:      5 * time.Minute,
		smCloseTimeout:      30 * time.Second,
		recoveryInterval:    time.Second,
		recoveryStepTimeout: 30 * time.Second,
		recoverySeedTimeout: 10 * time.Minute,
		recoveryNudge:       make(chan struct{}, 1),
		recoveryInflight:    make(map[string]struct{}),
		readyChan:           make(chan struct{}),
		members:             members,
		cfg:                 cfg,
		store:               store,
		closed:              make(chan struct{}),
		log:                 zap.S().Named("manager"),
		blockCache:          blockCache,
		incarnation:         newIncarnation(),
		scheduler:           scheduler,
	}
}

type Manager struct {
	store              store
	nh                 *raft.NodeHost
	mtx                sync.RWMutex
	reconcileMu        sync.Mutex
	wg                 sync.WaitGroup
	members            map[uint64]string
	closed             chan struct{}
	cfg                Config
	readyChan          chan struct{}
	readyOnce          sync.Once
	reconcileInterval  time.Duration
	cleanupInterval    time.Duration
	cleanupGracePeriod time.Duration
	cleanupTimeout     time.Duration
	smCloseTimeout     time.Duration
	log                *zap.SugaredLogger
	blockCache         *pebble.Cache
	scheduler          *ap.ConcurrencyLimitScheduler
	// incarnation identifies this process run. Recovery records carry the
	// incarnation that created them, so artifacts that cannot outlive their
	// process — a live on-demand snapshot has no stable identity — are
	// discarded on restart instead of being replayed stale.
	incarnation string

	// Recovery coordination (see recovery.go).
	bus                 RecoveryBus
	recoveryInterval    time.Duration
	recoveryStepTimeout time.Duration
	recoverySeedTimeout time.Duration
	recoveryNudge       chan struct{}
	recoveryMu          sync.Mutex
	recoveryInflight    map[string]struct{}
	progress            sync.Map
	// lifeCtx is cancelled by Close. Recovery work can block for minutes on a
	// large load, so it needs a cancellation channel of its own rather than
	// relying on the loop noticing m.closed between steps.
	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	// smClosed tracks, per shard, when the state machine started by this
	// manager has finished closing. See tableFSM.
	smMu     sync.Mutex
	smClosed map[uint64]chan struct{}
}

// newIncarnation returns an identifier unique to this process run.
func newIncarnation() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The value only ever needs to differ from the previous run's, so the
		// clock is a sufficient fallback.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

type Lease struct {
	ID    uint64    `json:"id"`
	Until time.Time `json:"until"`
}

func (m *Manager) LeaseTable(name string, lease time.Duration) error {
	key := storedTableName(name) + "/lease"
	get, err := m.store.Get(key)

	unclaimed := errors.Is(err, kv.ErrNotExist)
	if err != nil && !unclaimed {
		return err
	}

	l := Lease{}
	if !unclaimed {
		err = json.Unmarshal([]byte(get.Value), &l)
		if err != nil {
			return err
		}
	}

	if unclaimed || l.ID == m.cfg.NodeID || l.Until.Before(time.Now()) {
		l.ID = m.cfg.NodeID
		l.Until = time.Now().Add(lease)

		bts, err := json.Marshal(l)
		if err != nil {
			return err
		}
		_, err = m.store.Set(key, string(bts), get.Ver)
		if err != nil {
			return err
		}
		return nil
	}

	return serrors.ErrLeaseNotAcquired
}

// ReturnTable returns true if it was leased previously.
func (m *Manager) ReturnTable(name string) (bool, error) {
	key := storedTableName(name) + "/lease"
	get, err := m.store.Get(key)

	if errors.Is(err, kv.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	l := Lease{}
	err = json.Unmarshal([]byte(get.Value), &l)
	if err != nil {
		return false, err
	}

	if l.ID != m.cfg.NodeID {
		return false, nil
	}

	err = m.store.Delete(key, get.Ver)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (m *Manager) CreateTable(name string) (Table, error) {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	created, err := m.createTable(name)
	if err != nil {
		return Table{}, err
	}

	return created, m.startTable(created.Name, created.ClusterID)
}

func (m *Manager) createTable(name string) (Table, error) {
	storeName := storedTableName(name)
	exists, err := m.store.Exists(storeName)
	if err != nil {
		return Table{}, err
	}
	if exists {
		return Table{}, serrors.ErrTableExists
	}
	seq, err := m.incAndGetIDSeq()
	if err != nil {
		return Table{}, err
	}
	tab := Table{
		Name:      name,
		ClusterID: seq,
	}
	err = m.setTableVersion(tab, 0)
	if err != nil {
		if errors.Is(err, kv.ErrVersionMismatch) {
			return Table{}, serrors.ErrTableExists
		}
		return Table{}, err
	}
	return tab, nil
}

func (m *Manager) DeleteTable(name string) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()
	storeName := storedTableName(name)
	tab, err := m.store.Get(storeName)
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			return serrors.ErrTableNotFound
		}
		return err
	}

	return m.store.Delete(storeName, tab.Ver)
}

func storedTableName(name string) string {
	return fmt.Sprintf("%s%s", keyPrefix, name)
}

func (m *Manager) GetTable(name string) (ActiveTable, error) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	tab, _, err := m.getTableVersion(name)
	if err != nil {
		return ActiveTable{}, err
	}
	return tab.AsActive(m.nh), nil
}

func (m *Manager) GetTableByID(id uint64) (ActiveTable, error) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()
	tables, err := m.getTables()
	if err != nil {
		return ActiveTable{}, err
	}
	for _, t := range tables {
		if t.ClusterID == id {
			return t.AsActive(m.nh), nil
		}
	}
	return ActiveTable{}, serrors.ErrTableNotFound
}

func (m *Manager) GetTables() ([]Table, error) {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	tabs, err := m.getTables()
	if err != nil {
		return nil, err
	}
	rtabs := make([]Table, 0, len(tabs))
	for _, t := range tabs {
		rtabs = append(rtabs, t)
	}
	return rtabs, nil
}

func (m *Manager) Start() {
	m.wg.Go(m.reconcileLoop)
	m.wg.Go(m.cleanupLoop)
	m.wg.Go(m.recoveryLoop)
}

func (m *Manager) WaitUntilReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.closed:
		return serrors.ErrManagerClosed
	case <-m.readyChan:
		return nil
	}
}

func (m *Manager) Close() {
	close(m.closed)
	if m.lifeCancel != nil {
		m.lifeCancel()
	}
	m.wg.Wait()
}

func (m *Manager) reconcileLoop() {
	t := time.NewTimer(0)
	defer t.Stop()
	ready := false
	for {
		select {
		case <-m.closed:
			return
		case <-t.C:
			succeeded := m.reconcileOnce()
			if succeeded && !ready {
				m.readyOnce.Do(func() { close(m.readyChan) })
				ready = true
			}
			interval := m.reconcileInterval
			if !ready {
				interval = initialReconcileInterval
			}
			t.Reset(interval)
		}
	}
}

func (m *Manager) reconcileOnce() bool {
	if ls, ok := m.store.(leaderStore); ok && !ls.HasLeader() {
		return false
	}
	if err := m.reconcile(); err != nil {
		m.log.Errorf("reconcile failed: %v", err)
		return false
	}
	m.reconcileServingMembership()
	return m.tablesReady()
}

func (m *Manager) tablesReady() bool {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	tables, err := m.getTables()
	if err != nil {
		m.log.Errorf("failed to load tables while checking readiness: %v", err)
		return false
	}
	nhi := m.nh.GetNodeHostInfo(raft.NodeHostInfoOption{SkipLogInfo: true})
	if nhi == nil {
		return false
	}
	shards := make(map[uint64]raft.ShardInfo, len(nhi.ShardInfoList))
	for _, shard := range nhi.ShardInfoList {
		shards[shard.ShardID] = shard
	}
	// Readiness reflects only the serving shards. A recovery shard is managed
	// by the recovery loop and may legitimately be absent on this node (it only
	// exists on the coordinator and the learners it has admitted), so requiring
	// it here would wedge engine readiness for the whole recovery.
	for _, table := range tables {
		if table.ClusterID <= tableIDsRangeStart {
			continue
		}
		shard, ok := shards[table.ClusterID]
		if !ok || shard.Pending {
			return false
		}
	}
	return true
}

func (m *Manager) reconcile() error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	tabs, nhi, err := func() (map[string]Table, *raft.NodeHostInfo, error) {
		m.mtx.RLock()
		defer m.mtx.RUnlock()
		tabs, err := m.getTables()
		if err != nil {
			return nil, nil, err
		}
		nhi := m.nh.GetNodeHostInfo(raft.NodeHostInfoOption{SkipLogInfo: true})
		if nhi == nil {
			return nil, nil, serrors.ErrNodeHostInfoUnavailable
		}
		return tabs, nhi, nil
	}()
	if err != nil {
		return err
	}

	start, stop := diffTables(tabs, nhi.ShardInfoList)
	for id, tbl := range start {
		err = m.startTable(tbl.Name, id)
		if err != nil {
			return err
		}
	}

	for _, tab := range stop {
		err = m.stopTable(tab)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) cleanupLoop() {
	t := time.NewTicker(m.cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-m.closed:
			return
		case <-t.C:
			if ls, ok := m.store.(leaderStore); ok {
				if !ls.HasLeader() {
					m.log.Warnf("table store does not have a leader")
					continue
				}
			}
			err := m.cleanup()
			if err != nil {
				m.log.Errorf("cleanup failed: %v", err)
			}
		}
	}
}

func (m *Manager) cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cleanupTimeout)
	defer cancel()
	ls, err := m.store.GetAll(fmt.Sprintf("/cleanup/%d/*", m.cfg.NodeID))
	if err != nil {
		return err
	}
	for _, l := range ls {
		c := Cleanup{}
		if err := json.Unmarshal([]byte(l.Value), &c); err != nil {
			return err
		}
		if c.Created.Before(time.Now().Add(-m.cleanupGracePeriod)) {
			// Distributed data race guard
			if _, err := m.GetTableByID(c.ClusterID); !errors.Is(err, serrors.ErrTableNotFound) {
				m.log.Warnf("[%d:%d] cluster data cleanup skipped, table should not be deleted", c.ClusterID, m.cfg.NodeID)
				return m.store.Delete(l.Key, l.Ver)
			}

			if err := m.nh.SyncRemoveData(ctx, c.ClusterID, m.cfg.NodeID); err != nil {
				return err
			}
			// SyncRemoveData returning is not by itself proof that the state
			// machine has finished closing, and pulling the directory out from
			// under a live Pebble DB makes it report a fatal error — which
			// takes the whole process down. Leave the tombstone in place for
			// the next cycle rather than risk that.
			waitCtx, cancelWait := context.WithTimeout(ctx, m.smCloseTimeout)
			err := m.waitForStateMachineClosed(waitCtx, c.ClusterID)
			cancelWait()
			if err != nil {
				m.log.Warnf("[%d:%d] cluster data cleanup deferred: %v", c.ClusterID, m.cfg.NodeID, err)
				continue
			}
			if err := m.cfg.Table.FS.RemoveAll(c.SMDataPath); err != nil {
				return err
			}
			// The Raft data is gone, so this marker can no longer be needed to
			// select a restart role. Keep it until this point: membership state
			// alone cannot prove that the original AddNonVoting entry was compacted.
			if err := m.clearNonVotingMarker(c.ClusterID); err != nil {
				return fmt.Errorf("remove recovery role marker for shard %d: %w", c.ClusterID, err)
			}
			if err := m.store.Delete(l.Key, l.Ver); err != nil {
				return err
			}
			m.log.Infof("[%d:%d] cluster data cleaned", c.ClusterID, m.cfg.NodeID)
		}
	}
	return nil
}

func (m *Manager) incAndGetIDSeq() (uint64, error) {
	seq, err := m.store.Get(sequenceKey)
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			seq = kv.Pair{
				Key:   sequenceKey,
				Value: strconv.FormatUint(tableIDsRangeStart, 10),
				Ver:   0,
			}
		} else {
			return 0, err
		}
	}
	currSeq, err := strconv.ParseUint(seq.Value, 10, 64)
	if err != nil {
		return 0, err
	}
	next := currSeq + 1

	_, err = m.store.Set(seq.Key, strconv.FormatUint(next, 10), seq.Ver)
	return next, err
}

func (m *Manager) getTables() (map[string]Table, error) {
	tables := make(map[string]Table)
	all, err := m.store.GetAll(keyPrefix + "*")
	if err != nil {
		return nil, err
	}
	for _, v := range all {
		tab := Table{}
		err = json.Unmarshal([]byte(v.Value), &tab)
		if err != nil {
			return nil, err
		}
		tables[tab.Name] = tab
	}

	return tables, nil
}

// diffTables decides which shards this node should start and stop.
//
// Recovery shards are deliberately *known but never auto-started*: the recovery
// loop is the only thing allowed to start them, because the role a replica must
// take (single-voter coordinator vs. non-voting learner) is not derivable from
// the table entry, and starting a learner as a voter panics the Raft layer
// rather than returning an error.
func diffTables(tables map[string]Table, raftInfo []raft.ShardInfo) (toStart map[uint64]Table, toStop []uint64) {
	tableIDs := make(map[uint64]Table)
	knownIDs := make(map[uint64]struct{})
	for _, t := range tables {
		if t.ClusterID != 0 {
			tableIDs[t.ClusterID] = t
			knownIDs[t.ClusterID] = struct{}{}
		}
		if t.RecoverID != 0 {
			knownIDs[t.RecoverID] = struct{}{}
		}
	}
	raftTableIDs := make(map[uint64]struct{})
	for _, t := range raftInfo {
		raftTableIDs[t.ShardID] = struct{}{}
	}

	for tID, tName := range tableIDs {
		_, found := raftTableIDs[tID]
		if !found && tID > tableIDsRangeStart {
			if toStart == nil {
				toStart = make(map[uint64]Table)
			}
			toStart[tID] = tName
		}
	}

	for rID := range raftTableIDs {
		_, found := knownIDs[rID]
		if !found && rID > tableIDsRangeStart {
			toStop = append(toStop, rID)
		}
	}
	return
}

// tableFSM builds the on-disk state machine factory for a table shard.
//
// The created state machine is wrapped so the manager learns when its Pebble
// DB has actually been closed. cleanup() needs that signal: removing the state
// machine's directory while the DB is still open makes Pebble report a fatal
// error, and a fatal error from Pebble terminates the process.
func (m *Manager) tableFSM(name string) sm.CreateOnDiskStateMachineFunc {
	create := fsm.New(name, m.cfg.Table.DataDir, m.cfg.Table.FS, m.blockCache, m.scheduler, fsm.SnapshotRecoveryType(m.cfg.Table.RecoveryType), func(applied uint64) {
		if m.cfg.Table.AppliedIndexListener != nil {
			m.cfg.Table.AppliedIndexListener(name, applied)
		}
	})
	return func(shardID, replicaID uint64) sm.IOnDiskStateMachine {
		return &trackedStateMachine{
			IOnDiskStateMachine: create(shardID, replicaID),
			closed:              m.trackStateMachine(shardID),
		}
	}
}

// trackedStateMachine reports the moment the underlying state machine — and so
// its Pebble DB — is fully closed.
type trackedStateMachine struct {
	sm.IOnDiskStateMachine
	closed   chan struct{}
	closeOne sync.Once
}

func (t *trackedStateMachine) Close() error {
	defer t.closeOne.Do(func() { close(t.closed) })
	return t.IOnDiskStateMachine.Close()
}

// trackStateMachine registers a fresh completion channel for shardID and
// returns it. Restarting a shard replaces the previous channel.
func (m *Manager) trackStateMachine(shardID uint64) chan struct{} {
	ch := make(chan struct{})
	m.smMu.Lock()
	defer m.smMu.Unlock()
	if m.smClosed == nil {
		m.smClosed = make(map[uint64]chan struct{})
	}
	m.smClosed[shardID] = ch
	return ch
}

// waitForStateMachineClosed blocks until shardID's state machine has finished
// closing. A shard this process never started has nothing open, so it returns
// immediately.
func (m *Manager) waitForStateMachineClosed(ctx context.Context, shardID uint64) error {
	m.smMu.Lock()
	ch, tracked := m.smClosed[shardID]
	m.smMu.Unlock()
	if !tracked {
		return nil
	}
	select {
	case <-ch:
		m.smMu.Lock()
		if cur, ok := m.smClosed[shardID]; ok && cur == ch {
			delete(m.smClosed, shardID)
		}
		m.smMu.Unlock()
		return nil
	case <-ctx.Done():
		return fmt.Errorf("state machine of shard %d is still open: %w", shardID, ctx.Err())
	}
}

type replicaStartMode uint8

const (
	replicaBootstrap replicaStartMode = iota
	replicaRestart
	replicaJoin
)

type replicaStartOptions struct {
	Mode        replicaStartMode
	IsNonVoting bool
	Members     map[uint64]raft.Target
}

// startReplica is the one policy boundary for on-disk table replicas. In
// particular, an absent local log is not sufficient evidence that bootstrapping
// is safe: recovered serving shards must be joined.
func (m *Manager) startReplica(name string, id uint64, options replicaStartOptions) error {
	cfg := tableRaftConfig(m.cfg.NodeID, id, m.cfg.Table)
	cfg.IsNonVoting = options.IsNonVoting

	var (
		members map[uint64]raft.Target
		join    bool
	)
	switch options.Mode {
	case replicaBootstrap:
		members = options.Members
	case replicaRestart:
		members = map[uint64]raft.Target{}
	case replicaJoin:
		join = true
	default:
		return fmt.Errorf("unknown replica start mode %d", options.Mode)
	}
	if err := m.nh.StartOnDiskReplica(members, join, m.tableFSM(name), cfg); err != nil {
		if errors.Is(err, raft.ErrShardAlreadyExist) {
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) startTable(name string, id uint64) error {
	if m.nh.HasNodeInfo(id, m.cfg.NodeID) {
		// A shard swapped in by a learner-first recovery may still carry this
		// node's learner marker. Its local log can hold an AddNonVoting entry
		// naming this replica that has not been applied yet; re-applying that
		// entry against a voting replica panics rather than erroring, so start
		// non-voting.
		nonVoting, err := m.readNonVotingMarker(id)
		if err != nil {
			return fmt.Errorf("read recovery role marker for shard %d: %w", id, err)
		}
		return m.startReplica(name, id, replicaStartOptions{Mode: replicaRestart, IsNonVoting: nonVoting})
	}

	tbl, _, err := m.getTableVersion(name)
	if err != nil && !errors.Is(err, serrors.ErrTableNotFound) {
		return err
	}
	if err == nil && tbl.ClusterID == id && tbl.Recovered {
		// The recovered shard was originally bootstrapped by its coordinator.
		// A late member must join the existing configuration as a learner, never
		// recreate the configured voter set as a conflicting fresh bootstrap.
		//
		// Recovery may have already persisted AddNonVoting for this offline node
		// before the journal was deleted. Record the role before starting: replaying
		// that entry against a voting replica panics, while a learner safely
		// self-promotes once it receives the persisted promotion or a snapshot.
		if err := m.writeNonVotingMarker(id); err != nil {
			return fmt.Errorf("write recovery role marker for shard %d: %w", id, err)
		}
		return m.startReplica(name, id, replicaStartOptions{Mode: replicaJoin, IsNonVoting: true})
	}
	return m.startReplica(name, id, replicaStartOptions{Mode: replicaBootstrap, Members: m.members})
}

type Cleanup struct {
	Created    time.Time `json:"created"`
	ClusterID  uint64    `json:"cluster_id"`
	SMDataPath string    `json:"sm_data_path"`
}

// ApplySnapshot replays an incremental replication snapshot into the serving
// shard. It remains a narrow compatibility entry point while callers migrate to
// the recovery journal; journalled incremental recovery uses the same replay
// primitive after recording the artifact identity.
func (m *Manager) ApplySnapshot(ctx context.Context, name string, reader io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tbl, _, err := m.getTableVersion(name)
	if err != nil {
		return err
	}
	return m.readIntoTable(ctx, tbl.ClusterID, name, reader, true)
}

func (m *Manager) stopTable(clusterID uint64) error {
	v, err := m.nh.StaleRead(clusterID, fsm.PathRequest{})
	if err != nil {
		return err
	}
	pr := v.(*fsm.PathResponse)
	b, err := json.Marshal(&Cleanup{
		Created:    time.Now(),
		ClusterID:  clusterID,
		SMDataPath: pr.Path,
	})
	if err != nil {
		return err
	}
	key := fmt.Sprintf("/cleanup/%d/%d", m.cfg.NodeID, clusterID)
	c, err := m.store.Get(key)
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		return err
	}
	_, err = m.store.Set(key, string(b), c.Ver)
	if err != nil {
		return err
	}
	if err := m.nh.StopShard(clusterID); err != nil {
		return err
	}
	return nil
}

// Restore installs a replication snapshot. Replication snapshots must contain a
// validated terminal marker so source progress cannot advance from a truncated stream.
func (m *Manager) Restore(ctx context.Context, name string, reader io.Reader) error {
	return m.restore(ctx, name, reader, false)
}

// RestoreLegacy installs a maintenance backup created before replication snapshots
// required a terminal source-progress marker.
func (m *Manager) RestoreLegacy(ctx context.Context, name string, reader io.Reader) error {
	return m.restore(ctx, name, reader, true)
}

// restore loads a locally supplied stream through the learner-first recovery
// coordinator: the snapshot is loaded once, on this node, and the other members
// receive it through Dragonboat's snapshot path instead of replaying every
// command through their own Raft log.
func (m *Manager) restore(ctx context.Context, name string, reader io.Reader, legacy bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	recoveryID, err := m.incAndGetIDSeq()
	if err != nil {
		return err
	}
	rec := RecoveryRecord{
		Table:       name,
		Phase:       PhaseLoading,
		RecoveryID:  recoveryID,
		Coordinator: m.cfg.NodeID,
		Members:     sortedMemberIDs(m.members),
		Artifact: RecoveryArtifact{
			Type:      ArtifactLocal,
			StagePath: readerPath(reader),
			Legacy:    legacy,
		},
	}
	return m.coordinateRecovery(ctx, name, rec, reader)
}

// coordinateRecovery writes the journal entry that authorises the load and then
// runs the phase machine to completion. Writing the record before any shard is
// started is what keeps a crash from leaking a shard-ID sequence number.
func (m *Manager) coordinateRecovery(ctx context.Context, name string, rec RecoveryRecord, reader io.Reader) error {
	// Claim the table before writing the journal entry: otherwise the recovery
	// loop could pick the record up in the gap and this call would fail with
	// ErrRecoveryPending while the recovery actually proceeds.
	if !m.beginRecoveryWork(name) {
		return ErrRecoveryPending
	}
	defer m.endRecoveryWork(name)

	existing, ver, err := m.getRecovery(name)
	switch {
	case errors.Is(err, errNoRecovery):
		ver = 0
	case err != nil:
		return err
	case existing.Coordinator != m.cfg.NodeID:
		return ErrRecoveryConflict
	default:
		// Replace a stale record owned by this node; its shard, if any, is
		// tombstoned so the ID is not leaked.
		if err := m.abandonRecoveryLocked(existing, ver); err != nil {
			return err
		}
		ver = 0
	}
	if _, err := m.putRecovery(&rec, ver); err != nil {
		return err
	}
	if rec.RecoveryID != 0 {
		if err := m.pinRecoverID(name, rec.RecoveryID); err != nil {
			return err
		}
	}

	// A locally supplied restore is synchronous from the caller's point of
	// view, so wait out the phases that only make progress as peers catch up.
	// Holding the in-process slot throughout keeps the reconcile loop from
	// racing us for the same record.
	err = m.driveRecoveryLocked(ctx, name, reader)
	for errors.Is(err, ErrRecoveryPending) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.closed:
			return serrors.ErrManagerClosed
		case <-time.After(m.recoveryInterval):
		}
		err = m.driveRecoveryLocked(ctx, name, nil)
	}
	return err
}

// readerPath reports the on-disk path behind reader, when it has one, so a
// recovery interrupted mid-load can reopen it instead of being abandoned.
func readerPath(reader io.Reader) string {
	if p, ok := reader.(interface{ Path() string }); ok {
		return p.Path()
	}
	return ""
}

func (m *Manager) getTableVersion(name string) (Table, uint64, error) {
	v, err := m.store.Get(storedTableName(name))
	if err != nil {
		if errors.Is(err, kv.ErrNotExist) {
			return Table{}, 0, serrors.ErrTableNotFound
		}
		return Table{}, 0, err
	}
	tab := Table{}
	err = json.Unmarshal([]byte(v.Value), &tab)
	return tab, v.Ver, err
}

func (m *Manager) setTableVersion(tbl Table, version uint64) error {
	storeName := storedTableName(tbl.Name)
	bts, err := json.Marshal(&tbl)
	if err != nil {
		return err
	}
	_, err = m.store.Set(storeName, string(bts), version)
	if err != nil {
		return err
	}
	return nil
}

func (m *Manager) readIntoTable(ctx context.Context, id uint64, name string, reader io.Reader, requireTerminal bool) error {
	session := m.nh.GetNoOPSession(id)
	return readSnapshotWithTerminal(reader, name, m.cfg.Table.MaxInMemLogSize/2, requireTerminal, func(cmd *armadapb.Command) error {
		bb, err := cmd.MarshalVT()
		if err != nil {
			return err
		}

		backOff := backoff.NewExponentialBackOff()
		backOff.MaxElapsedTime = 0
		return backoff.Retry(func() error {
			if err := ctx.Err(); err != nil {
				return backoff.Permanent(err)
			}
			proposalCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err := m.nh.SyncPropose(proposalCtx, session, bb)
			if err != nil {
				if errors.Is(err, raft.ErrShardNotFound) {
					m.log.Warn("cluster not found recovery probably started on a different node")
					return backoff.Permanent(err)
				}
				m.log.Warnf("error proposing snapshot command: %v", err)
				return err
			}
			return nil
		}, backoff.WithContext(backOff, ctx))
	})
}

// readSnapshot validates a replication command snapshot and proposes its
// source-indexed data commands in bounded sequences. A final DUMMY command is
// required to durably advance source progress after every data sequence has applied.
func readSnapshot(reader io.Reader, tableName string, maxBatchSize uint64, propose func(*armadapb.Command) error) error {
	return readSnapshotWithTerminal(reader, tableName, maxBatchSize, true, propose)
}

func readSnapshotWithTerminal(reader io.Reader, tableName string, maxBatchSize uint64, requireTerminal bool, propose func(*armadapb.Command) error) error {
	const maxSnapshotRecordSize = 4 * 1024 * 1024

	msg := make([]byte, maxSnapshotRecordSize)
	batch := &armadapb.Command{
		Table: []byte(tableName),
		Type:  armadapb.Command_SEQUENCE,
	}
	flush := func() error {
		if len(batch.Sequence) == 0 {
			return nil
		}
		if err := propose(batch); err != nil {
			return err
		}
		clear(batch.Sequence)
		batch.Sequence = batch.Sequence[:0]
		return nil
	}

	var terminal *armadapb.Command
	var maxDataLeaderIndex uint64
	for {
		n, err := reader.Read(msg)
		if n > 0 {
			if terminal != nil {
				return fmt.Errorf("snapshot contains a record after its terminal marker")
			}

			cmd := &armadapb.Command{}
			if err := cmd.UnmarshalVT(msg[:n]); err != nil {
				return fmt.Errorf("unmarshal snapshot command: %w", err)
			}
			if !bytes.Equal(cmd.Table, []byte(tableName)) {
				return fmt.Errorf("snapshot command is for table %q, expected %q", cmd.Table, tableName)
			}

			switch cmd.Type {
			case armadapb.Command_DUMMY:
				if cmd.LeaderIndex == nil {
					return fmt.Errorf("snapshot terminal marker has no leader index")
				}
				if cmd.Kv != nil || len(cmd.Batch) != 0 || cmd.Txn != nil || len(cmd.Sequence) != 0 || len(cmd.RangeEnd) != 0 || cmd.PrevKvs || cmd.Count {
					return fmt.Errorf("snapshot terminal marker contains data")
				}
				if *cmd.LeaderIndex < maxDataLeaderIndex {
					return fmt.Errorf("snapshot terminal leader index %d is behind data index %d", *cmd.LeaderIndex, maxDataLeaderIndex)
				}
				terminal = cmd
			case armadapb.Command_PUT, armadapb.Command_DELETE:
				if cmd.Kv == nil || len(cmd.Kv.Key) == 0 {
					return fmt.Errorf("snapshot %s command has no key-value", cmd.Type)
				}
				if cmd.LeaderIndex == nil {
					if requireTerminal {
						return fmt.Errorf("snapshot %s command has no leader index", cmd.Type)
					}
				} else if *cmd.LeaderIndex > maxDataLeaderIndex {
					maxDataLeaderIndex = *cmd.LeaderIndex
				}
				batch.Sequence = append(batch.Sequence, cmd)
				if maxBatchSize > 0 && uint64(batch.SizeVT()) >= maxBatchSize {
					if err := flush(); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("snapshot contains unsupported command type %s", cmd.Type)
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			if terminal == nil {
				if requireTerminal {
					return fmt.Errorf("snapshot is missing its terminal marker")
				}
				return flush()
			}
			if err := flush(); err != nil {
				return err
			}
			return propose(terminal)
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

// waitForLeader blocks until clusterID has elected a leader. The budget is the
// recovery step timeout rather than a multiple of the reconcile interval: how
// long an election takes has nothing to do with how often tables reconcile, and
// tying the two made a fast reconcile interval abort legitimate elections.
func (m *Manager) waitForLeader(ctx context.Context, clusterID uint64) error {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()

	waitCtx, cancel := context.WithTimeout(ctx, m.recoveryStepTimeout)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-t.C:
			_, _, ok, _ := m.nh.GetLeaderID(clusterID)
			if ok {
				return nil
			}
		}
	}
}

// NotifyLogCompacted is called by the Raft event listener when the log for a
// shard has been compacted up to index. Only the leader proposes a Command_GC
// so that the GC horizon is replicated through the Raft log to all members of
// the group (and via the replication worker to follower regions). Followers
// ignore this call — they will apply the Command_GC when it arrives from the
// leader via normal replication.
func (m *Manager) NotifyLogCompacted(shardID uint64, index uint64) {
	// Compaction fires for every shard on this NodeHost, including the
	// metadata store. Proposing a table command into a shard that is not a
	// table panics its state machine, so resolve the shard first.
	if _, err := m.GetTableByID(shardID); err != nil {
		return
	}
	_, _, isLeader, err := m.nh.GetLeaderID(shardID)
	if err != nil || !isLeader {
		return
	}

	// For the leader, localIndex == leaderIndex, so index is the correct
	// leaderIndex value to embed in the GC command.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := &armadapb.Command{
			Type:        armadapb.Command_GC,
			LeaderIndex: &index,
		}
		bts, err := cmd.MarshalVT()
		if err != nil {
			m.log.Warnf("GC command marshal failed for shard %d index %d: %v", shardID, index, err)
			return
		}
		session := m.nh.GetNoOPSession(shardID)
		if _, err := m.nh.SyncPropose(ctx, session, bts); err != nil {
			m.log.Warnf("GC command proposal failed for shard %d index %d: %v", shardID, index, err)
		}
	}()
}

func tableRaftConfig(nodeID, clusterID uint64, cfg TableConfig) config.Config {
	return config.Config{
		ReplicaID:               nodeID,
		ShardID:                 clusterID,
		CheckQuorum:             true,
		OrderedConfigChange:     true,
		PreVote:                 true,
		ElectionRTT:             cfg.ElectionRTT,
		HeartbeatRTT:            cfg.HeartbeatRTT,
		SnapshotEntries:         cfg.SnapshotEntries,
		CompactionOverhead:      cfg.CompactionOverhead,
		MaxInMemLogSize:         cfg.MaxInMemLogSize,
		SnapshotCompressionType: config.Snappy,
	}
}
