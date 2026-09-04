// Copyright JAMF Software, LLC

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/armadakv/armada/armadaserver"
	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/security"
	"github.com/armadakv/armada/storage"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// setupCommonEnvironment sets up the common environment for both leader and follower modes.
// It initializes logging, sets up signal handling, and returns the logger and shutdown channel.
func setupCommonEnvironment() (*zap.Logger, *zap.SugaredLogger, chan os.Signal) {
	logger := rl.NewLogger(k.Bool("dev-mode"), k.String("log-level"))
	zap.ReplaceGlobals(logger)
	log := logger.Sugar().Named("root")
	engineLog := logger.Named("engine")
	setupDragonboatLogger(engineLog)

	// Check signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	return logger, log, shutdown
}

// createEngineConfig creates a common storage engine configuration for both leader and follower modes.
func createEngineConfig(engineLog *zap.Logger, appliedIndexListener func(table string, rev uint64)) (storage.Config, error) {
	raftAddress := k.String("raft.address")
	nodeID, initialMembers, err := parseInitialMembersList(k.Strings("raft.initial-members"), raftAddress)
	if err != nil {
		return storage.Config{}, fmt.Errorf("failed to parse raft.initial-members: %w", err)
	}

	if err := validateTLSTriplet("raft.tls"); err != nil {
		return storage.Config{}, err
	}
	if err := validateTLSTriplet("memberlist.tls"); err != nil {
		return storage.Config{}, err
	}

	// Gossip shares the raft UDP port (ALPN multiplexing). When memberlist.members
	// is not explicitly set, fall back to the raft peer addresses as gossip seeds
	// so operators don't need to repeat them under a different flag.
	gossipMembers := filterNonEmpty(k.Strings("memberlist.members"))
	if len(gossipMembers) == 0 {
		for _, addr := range initialMembers {
			gossipMembers = append(gossipMembers, addr)
		}
	}

	return storage.Config{
		Log:                 engineLog.Sugar(),
		ClientAddress:       k.String("api.advertise-address"),
		NodeID:              nodeID,
		InitialMembers:      initialMembers,
		WALDir:              k.String("raft.wal-dir"),
		NodeHostDir:         k.String("raft.node-host-dir"),
		RTTMillisecond:      uint64(k.Duration("raft.rtt").Milliseconds()),
		RaftAddress:         raftAddress,
		ListenAddress:       k.String("raft.listen-address"),
		EnableMetrics:       true,
		MaxReceiveQueueSize: uint64(k.Int64("raft.max-recv-queue-size")),
		MaxSendQueueSize:    uint64(k.Int64("raft.max-send-queue-size")),
		QUICUDPBufferSize:   k.Int("raft.quic-udp-buffer-size"),
		RaftTLS: security.TLSInfo{
			CertFile:       k.String("raft.tls-cert-file"),
			KeyFile:        k.String("raft.tls-key-file"),
			TrustedCAFile:  k.String("raft.tls-ca-file"),
			ClientCertAuth: k.String("raft.tls-ca-file") != "",
		},
		Gossip: storage.GossipConfig{
			AdvertiseAddress: k.String("memberlist.advertise-address"),
			InitialMembers:   gossipMembers,
			ClusterName:      k.String("memberlist.cluster-name"),
			NodeName:         k.String("memberlist.node-name"),
			TLS: security.TLSInfo{
				CertFile:       k.String("memberlist.tls-cert-file"),
				KeyFile:        k.String("memberlist.tls-key-file"),
				TrustedCAFile:  k.String("memberlist.tls-ca-file"),
				ClientCertAuth: k.String("memberlist.tls-ca-file") != "",
			},
		},
		Table: storage.TableConfig{
			FS:                   vfs.Default,
			ElectionRTT:          uint64(k.Int64("raft.election-rtt")),
			HeartbeatRTT:         uint64(k.Int64("raft.heartbeat-rtt")),
			SnapshotEntries:      uint64(k.Int64("raft.snapshot-entries")),
			CompactionOverhead:   uint64(k.Int64("raft.compaction-overhead")),
			MaxInMemLogSize:      uint64(k.Int64("raft.max-in-mem-log-size")),
			DataDir:              k.String("raft.state-machine-dir"),
			RecoveryType:         toRecoveryType(k.String("raft.snapshot-recovery-type")),
			BlockCacheSize:       k.Int64("storage.block-cache-size"),
			TableCacheSize:       k.Int("storage.table-cache-size"),
			AppliedIndexListener: appliedIndexListener,
		},
		Meta: storage.MetaConfig{
			ElectionRTT:        uint64(k.Int64("raft.election-rtt")),
			HeartbeatRTT:       uint64(k.Int64("raft.heartbeat-rtt")),
			SnapshotEntries:    uint64(k.Int64("raft.snapshot-entries")),
			CompactionOverhead: uint64(k.Int64("raft.compaction-overhead")),
			MaxInMemLogSize:    uint64(k.Int64("raft.max-in-mem-log-size")),
		},
	}, nil
}

// setupRESTServer creates and starts a REST server.
func setupRESTServer(log *zap.SugaredLogger) *armadaserver.RESTServer {
	addr, _, _ := resolveURL(k.String("rest.address"))
	hs := armadaserver.NewRESTServer(addr, k.Duration("rest.read-timeout"))
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("REST listenAndServe failed: %v", err)
		}
	}()
	return hs
}

// validateTLSTriplet checks that the cert, key, and CA flags for the given
// prefix are either all set or all empty.  A partial set is rejected because
// it produces a silently degraded TLS configuration.
func validateTLSTriplet(prefix string) error {
	cert := k.String(prefix + "-cert-file")
	key := k.String(prefix + "-key-file")
	ca := k.String(prefix + "-ca-file")
	set := 0
	for _, v := range []string{cert, key, ca} {
		if v != "" {
			set++
		}
	}
	if set > 0 && set < 3 {
		return fmt.Errorf("%s TLS configuration is incomplete: cert-file, key-file, and ca-file must all be set or all be empty", prefix)
	}
	return nil
}

// waitForShutdown waits for a shutdown signal and logs a message when received.
func waitForShutdown(shutdown chan os.Signal, log *zap.SugaredLogger) {
	<-shutdown
	log.Info("shutting down...")
}

// startEngine creates and starts the storage engine.
func startEngine(config storage.Config) (*storage.Engine, error) {
	engine, err := storage.New(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create engine: %w", err)
	}
	prometheus.MustRegister(engine)
	if err := engine.Start(); err != nil {
		return nil, fmt.Errorf("failed to start engine: %w", err)
	}
	return engine, nil
}

// waitForEngine waits for the engine to be ready and logs a message when it is.
func waitForEngine(ctx context.Context, engine *storage.Engine, log *zap.SugaredLogger) {
	if err := engine.WaitUntilReady(ctx); err != nil {
		log.Infof("engine failed to start: %v", err)
		return
	}
	log.Info("engine started")
}
