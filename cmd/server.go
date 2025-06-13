// Copyright JAMF Software, LLC

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	rl "github.com/armadakv/armada/log"
	"github.com/armadakv/armada/regattaserver"
	"github.com/armadakv/armada/storage"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// setupCommonEnvironment sets up the common environment for both leader and follower modes.
// It initializes logging, sets up signal handling, and returns the logger and shutdown channel.
func setupCommonEnvironment() (*zap.Logger, *zap.SugaredLogger, chan os.Signal, error) {
	logger := rl.NewLogger(viper.GetBool("dev-mode"), viper.GetString("log-level"))
	zap.ReplaceGlobals(logger)
	log := logger.Sugar().Named("root")
	engineLog := logger.Named("engine")
	setupDragonboatLogger(engineLog)

	if err := autoSetMaxprocs(log); err != nil {
		return nil, nil, nil, err
	}

	// Check signals
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	return logger, log, shutdown, nil
}

// createEngineConfig creates a common storage engine configuration for both leader and follower modes.
func createEngineConfig(engineLog *zap.Logger, appliedIndexListener func(table string, rev uint64)) (storage.Config, error) {
	initialMembers, err := parseInitialMembers(viper.GetStringMapString("raft.initial-members"))
	if err != nil {
		return storage.Config{}, fmt.Errorf("failed to parse raft.initial-members: %w", err)
	}

	return storage.Config{
		Log:                 engineLog.Sugar(),
		ClientAddress:       viper.GetString("api.advertise-address"),
		NodeID:              viper.GetUint64("raft.node-id"),
		InitialMembers:      initialMembers,
		WALDir:              viper.GetString("raft.wal-dir"),
		NodeHostDir:         viper.GetString("raft.node-host-dir"),
		RTTMillisecond:      uint64(viper.GetDuration("raft.rtt").Milliseconds()),
		RaftAddress:         viper.GetString("raft.address"),
		ListenAddress:       viper.GetString("raft.listen-address"),
		EnableMetrics:       true,
		MaxReceiveQueueSize: viper.GetUint64("raft.max-recv-queue-size"),
		MaxSendQueueSize:    viper.GetUint64("raft.max-send-queue-size"),
		Gossip: storage.GossipConfig{
			BindAddress:      viper.GetString("memberlist.address"),
			AdvertiseAddress: viper.GetString("memberlist.advertise-address"),
			InitialMembers:   viper.GetStringSlice("memberlist.members"),
			ClusterName:      viper.GetString("memberlist.cluster-name"),
			NodeName:         viper.GetString("memberlist.node-name"),
		},
		Table: storage.TableConfig{
			FS:                   vfs.Default,
			ElectionRTT:          viper.GetUint64("raft.election-rtt"),
			HeartbeatRTT:         viper.GetUint64("raft.heartbeat-rtt"),
			SnapshotEntries:      viper.GetUint64("raft.snapshot-entries"),
			CompactionOverhead:   viper.GetUint64("raft.compaction-overhead"),
			MaxInMemLogSize:      viper.GetUint64("raft.max-in-mem-log-size"),
			DataDir:              viper.GetString("raft.state-machine-dir"),
			RecoveryType:         toRecoveryType(viper.GetString("raft.snapshot-recovery-type")),
			BlockCacheSize:       viper.GetInt64("storage.block-cache-size"),
			TableCacheSize:       viper.GetInt("storage.table-cache-size"),
			AppliedIndexListener: appliedIndexListener,
		},
		Meta: storage.MetaConfig{
			ElectionRTT:        viper.GetUint64("raft.election-rtt"),
			HeartbeatRTT:       viper.GetUint64("raft.heartbeat-rtt"),
			SnapshotEntries:    viper.GetUint64("raft.snapshot-entries"),
			CompactionOverhead: viper.GetUint64("raft.compaction-overhead"),
			MaxInMemLogSize:    viper.GetUint64("raft.max-in-mem-log-size"),
		},
	}, nil
}

// setupRESTServer creates and starts a REST server.
func setupRESTServer(log *zap.SugaredLogger) *regattaserver.RESTServer {
	addr, _, _ := resolveURL(viper.GetString("rest.address"))
	hs := regattaserver.NewRESTServer(addr, viper.GetDuration("rest.read-timeout"))
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("REST listenAndServe failed: %v", err)
		}
	}()
	return hs
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
