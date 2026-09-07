// Copyright JAMF Software, LLC

// Package replication implements cross-cluster, pull-based replication for Armada.
// A Manager owns one replication Worker per leased table. Each Worker polls the leader
// cluster's Log.Replicate RPC, converts the received entries into logical commands, and
// re-proposes them into the follower's own Raft group so that the same MVCC revision is
// applied consistently across all regions.
package replication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/table"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/proto"
)

const replicationStoreID = 2000

type WorkerConfig struct {
	PollInterval        time.Duration
	LeaseInterval       time.Duration
	LogRPCTimeout       time.Duration
	SnapshotRPCTimeout  time.Duration
	MaxRecoveryInFlight int64
}

type Config struct {
	ReconcileInterval time.Duration
	Workers           WorkerConfig
}

type replicationManagerStore interface {
	GetAllValues(key string) ([]string, error)
	Get(key string) (kv.Pair, error)
	Set(key string, sprintf string, ver uint64) (kv.Pair, error)
	Delete(key string, ver uint64) error
}

type sourceTableState struct {
	ClusterID        uint64 `json:"cluster_id"`
	NeedsFullRestore bool   `json:"needs_full_restore"`
}

func sourceTableStateKey(name string) string {
	return fmt.Sprintf("source-tables/%s", name)
}

// NewManager constructs a new replication Manager out of tables.Manager, dragonboat.NodeHost and replication API grpc.ClientConn.
func NewManager(
	e *storage.Engine,
	queue *storage.IndexNotificationQueue,
	conn *grpc.ClientConn,
	snapshotGetter SnapshotObjectGetter,
	snapshotQuery SnapshotQueryResolver,
	cfg Config,
) *Manager {
	replicationLog := zap.S().Named("replication")
	if snapshotQuery == nil {
		snapshotQuery = NewGRPCSnapshotQueryResolver(armadapb.NewSnapshotClient(conn))
	}

	replicationIndexGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "regatta_replication_index",
			Help: "Regatta replication index",
		}, []string{"role", "table"},
	)
	replicationLeaseGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "regatta_replication_leased",
			Help: "Regatta replication has the worker table leased",
		}, []string{"table"},
	)

	return &Manager{
		reconcileInterval: cfg.ReconcileInterval,
		engine:            e,
		metadataClient:    armadapb.NewMetadataClient(conn),
		factory: &workerFactory{
			queue:             queue,
			reconcileInterval: cfg.ReconcileInterval,
			pollInterval:      cfg.Workers.PollInterval,
			leaseInterval:     cfg.Workers.LeaseInterval,
			logTimeout:        cfg.Workers.LogRPCTimeout,
			snapshotTimeout:   cfg.Workers.SnapshotRPCTimeout,
			recoverySemaphore: semaphore.NewWeighted(cfg.Workers.MaxRecoveryInFlight),
			engine:            e,
			store: &kv.RaftStore{
				NodeHost:  e.NodeHost,
				ClusterID: replicationStoreID,
			},
			log:            replicationLog,
			logClient:      armadapb.NewLogClient(conn),
			snapshotQuery:  snapshotQuery,
			snapshotGetter: snapshotGetter,
			metrics: struct {
				replicationIndex  *prometheus.GaugeVec
				replicationLeased *prometheus.GaugeVec
			}{replicationIndex: replicationIndexGauge, replicationLeased: replicationLeaseGauge},
		},
		workers: struct {
			registry map[string]*worker
			wg       sync.WaitGroup
		}{
			registry: make(map[string]*worker),
		},
		sources: make(map[string]sourceTableState),
		log:     replicationLog.Named("manager"),
		closer:  make(chan struct{}),
	}
}

// Manager schedules replication workers.
type Manager struct {
	reconcileInterval time.Duration
	engine            *storage.Engine
	metadataClient    armadapb.MetadataClient
	factory           *workerFactory
	workers           struct {
		registry map[string]*worker
		wg       sync.WaitGroup
	}
	sources map[string]sourceTableState
	log     *zap.SugaredLogger
	closer  chan struct{}
}

func (m *Manager) Describe(descs chan<- *prometheus.Desc) {
	m.factory.metrics.replicationIndex.Describe(descs)
	m.factory.metrics.replicationLeased.Describe(descs)
}

func (m *Manager) Collect(metrics chan<- prometheus.Metric) {
	m.factory.metrics.replicationIndex.Collect(metrics)
	m.factory.metrics.replicationLeased.Collect(metrics)
}

// Start starts the replication manager goroutine, Close will stop it.
func (m *Manager) Start() error {
	if rs, ok := m.factory.store.(*kv.RaftStore); ok {
		err := rs.Start(kv.RaftConfig{
			NodeID:             m.engine.Config().NodeID,
			HeartbeatRTT:       5,
			ElectionRTT:        100,
			SnapshotEntries:    1000,
			CompactionOverhead: 100,
			MaxInMemLogSize:    1024 * 1024,
			InitialMembers:     m.engine.Config().InitialMembers,
		})
		if err != nil {
			return err
		}
	}
	go func() {
		t := time.NewTicker(m.reconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
			case <-m.closer:
				m.log.Info("replication stopped")
				return
			}
			if err := m.reconcileTables(); err != nil {
				m.log.Errorf("failed to reconcile tables: %v", err)
				continue
			}
			if err := m.reconcileWorkers(); err != nil {
				m.log.Errorf("failed to reconcile replication workers: %v", err)
				continue
			}
		}
	}()
	return nil
}

func (m *Manager) reconcileTables() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := m.metadataClient.Get(ctx, &armadapb.MetadataRequest{})
	if err != nil {
		return err
	}
	followerTables, err := m.engine.GetTables()
	if err != nil {
		return err
	}

	leaderTables := make(map[string]uint64, len(response.GetTables()))
	for _, leaderTable := range response.GetTables() {
		if leaderTable.GetClusterId() == 0 {
			return fmt.Errorf("leader metadata for table %q has no source shard identity", leaderTable.GetName())
		}
		leaderTables[leaderTable.GetName()] = leaderTable.GetClusterId()
	}
	followerByName := make(map[string]table.Table, len(followerTables))
	for _, followerTable := range followerTables {
		followerByName[followerTable.Name] = followerTable
	}

	sources := make(map[string]sourceTableState, len(leaderTables))
	for name, sourceID := range leaderTables {
		state, found, err := m.loadSourceTableState(name)
		if err != nil {
			return err
		}
		_, existsLocally := followerByName[name]
		if !existsLocally || !found || state.ClusterID != sourceID {
			state = sourceTableState{ClusterID: sourceID, NeedsFullRestore: true}
			if err := m.recreateFollowerTable(name, state, existsLocally); err != nil {
				return err
			}
		}
		sources[name] = state
	}

	for name := range followerByName {
		if _, existsOnLeader := leaderTables[name]; existsOnLeader {
			continue
		}
		if m.hasWorker(name) {
			m.stopWorker(m.workers.registry[name])
		}
		if err := m.engine.DeleteTable(name); err != nil && !errors.Is(err, serrors.ErrTableNotFound) {
			return err
		}
		if err := m.deleteSourceTableState(name); err != nil {
			return err
		}
	}
	m.sources = sources
	return nil
}

func (m *Manager) recreateFollowerTable(name string, state sourceTableState, existsLocally bool) error {
	if m.hasWorker(name) {
		m.stopWorker(m.workers.registry[name])
	}
	if existsLocally {
		if err := m.engine.DeleteTable(name); err != nil && !errors.Is(err, serrors.ErrTableNotFound) {
			return err
		}
	}
	if _, err := m.engine.CreateTable(name); err != nil && !errors.Is(err, serrors.ErrTableExists) {
		return err
	}
	return m.saveSourceTableState(name, state)
}

func (m *Manager) loadSourceTableState(name string) (sourceTableState, bool, error) {
	pair, err := m.factory.store.Get(sourceTableStateKey(name))
	if errors.Is(err, kv.ErrNotExist) {
		return sourceTableState{}, false, nil
	}
	if err != nil {
		return sourceTableState{}, false, err
	}
	state := sourceTableState{}
	if err := json.Unmarshal([]byte(pair.Value), &state); err != nil {
		return sourceTableState{}, false, fmt.Errorf("decode source state for table %q: %w", name, err)
	}
	return state, true, nil
}

func (m *Manager) saveSourceTableState(name string, state sourceTableState) error {
	pair, err := m.factory.store.Get(sourceTableStateKey(name))
	if err != nil && !errors.Is(err, kv.ErrNotExist) {
		return err
	}
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = m.factory.store.Set(sourceTableStateKey(name), string(value), pair.Ver)
	return err
}

func (m *Manager) deleteSourceTableState(name string) error {
	pair, err := m.factory.store.Get(sourceTableStateKey(name))
	if errors.Is(err, kv.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.factory.store.Delete(sourceTableStateKey(name), pair.Ver)
}

func markSourceTableRestored(store replicationManagerStore, name string, sourceID uint64) error {
	key := sourceTableStateKey(name)
	pair, err := store.Get(key)
	if err != nil {
		return err
	}
	state := sourceTableState{}
	if err := json.Unmarshal([]byte(pair.Value), &state); err != nil {
		return fmt.Errorf("decode source state for table %q: %w", name, err)
	}
	if state.ClusterID != sourceID {
		return fmt.Errorf("source identity changed for table %q", name)
	}
	state.NeedsFullRestore = false
	value, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = store.Set(key, string(value), pair.Ver)
	return err
}

func (m *Manager) reconcileWorkers() error {
	tbs, err := m.engine.GetTables()
	if err != nil {
		return err
	}

	for _, tbl := range tbs {
		source, ok := m.sources[tbl.Name]
		if !ok {
			m.log.Warnf("skipping table %q without a persisted source identity", tbl.Name)
			continue
		}
		if existing, ok := m.workers.registry[tbl.Name]; ok && (existing.sourceID != source.ClusterID || existing.forceRecovery.Load() != source.NeedsFullRestore) {
			m.stopWorker(existing)
		}
		if !m.hasWorker(tbl.Name) {
			m.startWorker(m.factory.create(tbl.Name, source))
		}
	}

	var toStop []*worker

	for name, w := range m.workers.registry {
		if !slices.ContainsFunc(tbs, func(t table.Table) bool {
			return t.Name == name
		}) {
			toStop = append(toStop, w)
		}
	}

	for _, w := range toStop {
		m.stopWorker(w)
	}
	return nil
}

// Close will stop replication goroutine - could be called just once.
func (m *Manager) Close() {
	m.closer <- struct{}{}
	for _, worker := range m.workers.registry {
		m.stopWorker(worker)
	}
	m.workers.wg.Wait()
}

func (m *Manager) hasWorker(name string) bool {
	_, ok := m.workers.registry[name]
	return ok
}

func (m *Manager) startWorker(worker *worker) {
	m.log.Infof("launching replication for table %s", worker.table)
	m.workers.registry[worker.table] = worker
	m.workers.wg.Add(1)
	worker.Start()
}

func (m *Manager) stopWorker(worker *worker) {
	m.log.Infof("stopping replication for table %s", worker.table)
	worker.Close()
	m.workers.wg.Done()
	delete(m.workers.registry, worker.table)
}
