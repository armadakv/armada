// Copyright JAMF Software, LLC

// Package storage is the core runtime engine for Armada.
// It wraps the Raft node host, the gossip-based cluster registry, a Raft-backed
// metadata store, and the per-table state machine manager into a single Engine that
// the server layers (armadaserver, replication) build on top of.
package storage

import (
	"context"
	"crypto/tls"
	"fmt"
	"iter"
	"strconv"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/config"
	"github.com/armadakv/armada/raft/transport"
	"github.com/armadakv/armada/storage/cluster"
	"github.com/armadakv/armada/storage/kv"
	"github.com/armadakv/armada/storage/logreader"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/armada/util/iterx"
	"github.com/armadakv/armada/version"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	defaultQueryTimeout = 5 * time.Second
	tableStoreID        = 1000
)

func New(cfg Config) (*Engine, error) {
	e := &Engine{
		cfg:  cfg,
		log:  cfg.Log,
		stop: make(chan struct{}),
	}
	e.events = &events{eventsCh: make(chan any, 1), stopc: make(chan struct{}), donec: make(chan struct{}), engine: e}

	// Create the shared QUIC transport that raft and gossip will both use.
	// Use the listen address when set, otherwise fall back to the raft address.
	listenAddr := cfg.ListenAddress
	if listenAddr == "" {
		listenAddr = cfg.RaftAddress
	}
	sharedQT, err := transport.New(listenAddr, cfg.QUICUDPBufferSize)
	if err != nil {
		return nil, fmt.Errorf("failed to create shared QUIC transport: %w", err)
	}
	e.sharedQT = sharedQT

	// Build TLS for gossip early so errors surface before raft starts.
	var gossipServerTLS, gossipClientTLS *tls.Config
	if !cfg.Gossip.TLS.Empty() {
		gossipServerTLS, err = cfg.Gossip.TLS.ServerConfig()
		if err != nil {
			_ = sharedQT.Close()
			return nil, fmt.Errorf("gossip server TLS: %w", err)
		}
		gossipClientTLS, err = cfg.Gossip.TLS.ClientConfig()
		if err != nil {
			_ = sharedQT.Close()
			return nil, fmt.Errorf("gossip client TLS: %w", err)
		}
	}
	gossipAdvAddr := cfg.Gossip.AdvertiseAddress
	if gossipAdvAddr == "" {
		gossipAdvAddr = cfg.RaftAddress
	}

	// Create the gossip cluster before raft so that it can be passed directly
	// as the raft node registry. Until the NodeHost is assigned (below),
	// clusterInfo returns minimal metadata — gossip just broadcasts empty shard
	// info initially.
	// The gossip node name must be unique per member; fall back to the raft
	// address (always unique) when not explicitly configured.
	gossipNodeName := cfg.Gossip.NodeName
	if gossipNodeName == "" {
		gossipNodeName = cfg.RaftAddress
	}
	clst, err := cluster.New(gossipAdvAddr, cfg.Gossip.ClusterName, gossipNodeName, gossipServerTLS, gossipClientTLS, sharedQT, e.clusterInfo)
	if err != nil {
		_ = sharedQT.Close()
		return nil, fmt.Errorf("failed to bootstrap gossip cluster: %w", err)
	}
	e.Cluster = clst

	nh, err := createNodeHost(e, sharedQT, clst)
	if err != nil {
		_ = clst.Close()
		_ = sharedQT.Close()
		return nil, fmt.Errorf("failed to start raft nodehost: %w", err)
	}
	e.NodeHost = nh

	// All ALPN listeners are now registered; start the shared QUIC listener.
	if err := sharedQT.Serve(); err != nil {
		nh.Close()
		_ = clst.Close()
		_ = sharedQT.Close()
		return nil, fmt.Errorf("failed to start shared QUIC listener: %w", err)
	}
	e.tableStore = &kv.RaftStore{
		NodeHost:  nh,
		ClusterID: tableStoreID,
	}
	e.Manager = table.NewManager(
		nh,
		cfg.InitialMembers,
		e.tableStore,
		table.Config{
			NodeID: cfg.NodeID,
			Table:  table.TableConfig(cfg.Table),
			Meta:   table.MetaConfig(cfg.Meta),
		},
	)
	e.LogReader = &logreader.Simple{LogQuerier: nh}
	e.disk = newDiskMetrics(cfg.NodeHostDir, cfg.WALDir, cfg.Table.DataDir)
	return e, nil
}

// Describe implements prometheus.Collector.
func (e *Engine) Describe(ch chan<- *prometheus.Desc) {
	e.disk.Describe(ch)
}

// Collect implements prometheus.Collector.
func (e *Engine) Collect(ch chan<- prometheus.Metric) {
	e.disk.Collect(ch)
}

type Engine struct {
	*raft.NodeHost
	*table.Manager
	cfg        Config
	log        *zap.SugaredLogger
	events     *events
	stop       chan struct{}
	LogReader  logreader.Interface
	Cluster    *cluster.Cluster
	tableStore *kv.RaftStore
	disk       *diskMetrics
	sharedQT   *transport.Shared
}

func (e *Engine) Start() error {
	e.Cluster.Start(e.cfg.Gossip.InitialMembers)
	if err := e.tableStore.Start(kv.RaftConfig{
		NodeID:             e.cfg.NodeID,
		ElectionRTT:        e.cfg.Meta.ElectionRTT,
		HeartbeatRTT:       e.cfg.Meta.HeartbeatRTT,
		SnapshotEntries:    e.cfg.Meta.SnapshotEntries,
		CompactionOverhead: e.cfg.Meta.CompactionOverhead,
		MaxInMemLogSize:    e.cfg.Meta.MaxInMemLogSize,
		InitialMembers:     e.cfg.InitialMembers,
	}); err != nil {
		return err
	}
	e.Manager.Start()
	e.events.started.Store(true)
	go e.events.dispatchEvents()
	return nil
}

func (e *Engine) Close() error {
	close(e.stop)
	e.Manager.Close()
	if e.Cluster != nil {
		_ = e.Cluster.Close()
	}
	e.NodeHost.Close()
	if e.sharedQT != nil {
		_ = e.sharedQT.Close()
	}
	if e.events.started.Load() {
		<-e.events.donec
	}
	return nil
}

func (e *Engine) Range(ctx context.Context, req *armadapb.RangeRequest) (*armadapb.RangeResponse, error) {
	t, err := e.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	rng, err := withDefaultTimeout(ctx, req, t.Range)
	if err != nil {
		return nil, err
	}
	rng.Header = e.getHeader(nil, t.ClusterID)
	return rng, nil
}

func (e *Engine) IterateRange(ctx context.Context, req *armadapb.RangeRequest) (iter.Seq[*armadapb.RangeResponse], error) {
	t, err := e.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	it, err := withDefaultTimeout(ctx, req, t.Iterator)
	if err != nil {
		return nil, err
	}
	return iterx.Map(it, func(s *armadapb.ResponseOp_Range) *armadapb.RangeResponse {
		return &armadapb.RangeResponse{
			Header: e.getHeader(nil, t.ClusterID),
			Kvs:    s.Kvs,
			More:   s.More,
			Count:  s.Count,
		}
	}), nil
}

func (e *Engine) Put(ctx context.Context, req *armadapb.PutRequest) (*armadapb.PutResponse, error) {
	t, err := e.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	put, err := withDefaultTimeout(ctx, req, t.Put)
	if err != nil {
		return nil, err
	}
	put.Header = e.getHeader(put.Header, t.ClusterID)
	return put, nil
}

func (e *Engine) Delete(ctx context.Context, req *armadapb.DeleteRangeRequest) (*armadapb.DeleteRangeResponse, error) {
	t, err := e.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	del, err := withDefaultTimeout(ctx, req, t.Delete)
	if err != nil {
		return nil, err
	}
	del.Header = e.getHeader(del.Header, t.ClusterID)
	return del, nil
}

func (e *Engine) Txn(ctx context.Context, req *armadapb.TxnRequest) (*armadapb.TxnResponse, error) {
	t, err := e.GetTable(string(req.Table))
	if err != nil {
		return nil, err
	}
	tx, err := withDefaultTimeout(ctx, req, t.Txn)
	if err != nil {
		return nil, err
	}
	tx.Header = e.getHeader(tx.Header, t.ClusterID)
	return tx, nil
}

func (e *Engine) MemberList(ctx context.Context, r *armadapb.MemberListRequest) (*armadapb.MemberListResponse, error) {
	return withDefaultTimeout(ctx, r, func(ctx context.Context, r *armadapb.MemberListRequest) (*armadapb.MemberListResponse, error) {
		nodes := e.Cluster.Nodes()
		res := &armadapb.MemberListResponse{Cluster: e.Cluster.Name(), Members: make([]*armadapb.Member, len(nodes))}
		for i, node := range nodes {
			res.Members[i] = &armadapb.Member{
				Id:         strconv.FormatUint(node.NodeID, 10),
				Name:       node.Name,
				PeerURLs:   []string{node.RaftAddress},
				ClientURLs: []string{node.ClientAddress},
			}
		}
		return res, nil
	})
}

func (e *Engine) Status(ctx context.Context, r *armadapb.StatusRequest) (*armadapb.StatusResponse, error) {
	return withDefaultTimeout(ctx, r, func(ctx context.Context, _ *armadapb.StatusRequest) (*armadapb.StatusResponse, error) {
		res := &armadapb.StatusResponse{
			Id:      strconv.FormatUint(e.cfg.NodeID, 10),
			Version: version.Version,
			Tables:  make(map[string]*armadapb.TableStatus),
		}
		tables, err := e.GetTables()
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
		}
		for _, t := range tables {
			at, err := e.GetTable(t.Name)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", t.Name, err.Error()))
				continue
			}
			index, err := at.LocalIndex(ctx, false)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", t.Name, err.Error()))
				continue
			}
			lid, term, _, err := e.GetLeaderID(at.ClusterID)
			if err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", t.Name, err.Error()))
				continue
			}
			res.Tables[at.Name] = &armadapb.TableStatus{
				Leader:           strconv.FormatUint(lid, 10),
				RaftIndex:        index.Index,
				RaftTerm:         term,
				RaftAppliedIndex: index.Index,
			}
		}
		return res, nil
	})
}

func (e *Engine) getHeader(header *armadapb.ResponseHeader, shardID uint64) *armadapb.ResponseHeader {
	if header == nil {
		header = &armadapb.ResponseHeader{}
	}
	header.ReplicaId = e.cfg.NodeID
	header.ShardId = shardID
	info := e.Cluster.ShardInfo(shardID)
	header.RaftTerm = info.Term
	header.RaftLeaderId = info.LeaderID
	return header
}

func (e *Engine) clusterInfo() cluster.Info {
	info := cluster.Info{
		NodeID:        e.cfg.NodeID,
		RaftAddress:   e.cfg.RaftAddress,
		ClientAddress: e.cfg.ClientAddress,
	}
	if e.NodeHost == nil {
		return info
	}
	info.NodeHostID = e.ID()
	if nhi := e.GetNodeHostInfo(raft.DefaultNodeHostInfoOption); nhi != nil {
		info.ShardInfoList = nhi.ShardInfoList
		info.LogInfo = nhi.LogInfo
	}
	return info
}

func (e *Engine) WaitUntilReady(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return e.tableStore.WaitForLeader(ctx)
	})
	return eg.Wait()
}

func (e *Engine) Config() Config {
	return e.cfg
}

func createNodeHost(e *Engine, sharedQT *transport.Shared, clst *cluster.Cluster) (*raft.NodeHost, error) {
	nhc := config.NodeHostConfig{
		WALDir:              e.cfg.WALDir,
		NodeHostDir:         e.cfg.NodeHostDir,
		RTTMillisecond:      e.cfg.RTTMillisecond,
		RaftAddress:         e.cfg.RaftAddress,
		ListenAddress:       e.cfg.ListenAddress,
		EnableMetrics:       true,
		MaxReceiveQueueSize: e.cfg.MaxReceiveQueueSize,
		MaxSendQueueSize:    e.cfg.MaxSendQueueSize,
		QUICUDPBufferSize:   e.cfg.QUICUDPBufferSize,
		SystemEventListener: e.events,
		RaftEventListener:   e.events,
	}
	nhc.Expert.LogDB = buildLogDBConfig()

	if !e.cfg.RaftTLS.Empty() {
		serverTLS, err := e.cfg.RaftTLS.ServerConfig()
		if err != nil {
			return nil, fmt.Errorf("raft server TLS: %w", err)
		}
		clientTLS, err := e.cfg.RaftTLS.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("raft client TLS: %w", err)
		}
		nhc.ServerTLS = serverTLS
		nhc.ClientTLS = clientTLS
	}

	if e.cfg.FS != nil {
		nhc.Expert.FS = e.cfg.FS
	}

	err := nhc.Prepare()
	if err != nil {
		return nil, err
	}

	nh, err := raft.NewNodeHost(nhc, raft.WithTransportOptions(transport.WithShared(sharedQT)), raft.WithRegistry(clst))
	if err != nil {
		return nil, err
	}
	return nh, nil
}

func buildLogDBConfig() config.LogDBConfig {
	cfg := config.GetSmallMemLogDBConfig()
	cfg.KVRecycleLogFileNum = 4
	cfg.KVMaxBytesForLevelBase = 128 * 1024 * 1024
	return cfg
}

func withDefaultTimeout[R any, S any](ctx context.Context, req R, f func(context.Context, R) (S, error)) (S, error) {
	if _, ok := ctx.Deadline(); !ok {
		dctx, cancel := context.WithTimeout(ctx, defaultQueryTimeout)
		defer cancel()
		ctx = dctx
	}
	return f(ctx, req)
}
