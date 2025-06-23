// Copyright JAMF Software, LLC

package cmd

import (
	"errors"
	"fmt"
	"time"

	"github.com/armadakv/armada/regattapb"
	"github.com/armadakv/armada/regattaserver"
	"github.com/armadakv/armada/replication"
	"github.com/armadakv/armada/security"
	"github.com/armadakv/armada/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

func init() {
	// Base flags
	followerCmd.PersistentFlags().AddFlagSet(rootFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(apiFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(restFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(raftFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(memberlistFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(storageFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(maintenanceFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(tablesFlagSet)
	followerCmd.PersistentFlags().AddFlagSet(experimentalFlagSet)

	// Replication flags
	followerCmd.PersistentFlags().String("replication.leader-address", "localhost:8444", "Address of the leader replication API to connect to.")
	followerCmd.PersistentFlags().Duration("replication.keepalive-time", 1*time.Minute, "After a duration of this time if the replication client doesn't see any activity it pings the server to see if the transport is still alive. If set below 10s, a minimum value of 10s will be used instead.")
	followerCmd.PersistentFlags().Duration("replication.keepalive-timeout", 10*time.Second, "After having pinged for keepalive check, the replication client waits for a duration of Timeout and if no activity is seen even after that the connection is closed.")
	followerCmd.PersistentFlags().String("replication.cert-filename", "hack/replication/client.crt", "Path to the client certificate.")
	followerCmd.PersistentFlags().String("replication.key-filename", "hack/replication/client.key", "Path to the client private key file.")
	followerCmd.PersistentFlags().String("replication.ca-filename", "hack/replication/ca.crt", "Path to the client CA cert file. The CA file is used to verify server authority.")
	followerCmd.PersistentFlags().Bool("replication.insecure-skip-verify", false, "InsecureSkipVerify controls whether a client verifies the server's certificate chain and host name. If InsecureSkipVerify is true, crypto/tls accepts any certificate presented by the server and any host name in that certificate.")
	followerCmd.PersistentFlags().String("replication.server-name", "", "ServerName ensures the cert matches the given host in case of discovery/virtual hosting.")
	followerCmd.PersistentFlags().Duration("replication.poll-interval", 1*time.Second, "Replication interval in seconds, the leader poll time.")
	followerCmd.PersistentFlags().Duration("replication.reconcile-interval", 30*time.Second, "Replication interval of tables reconciliation (workers startup/shutdown).")
	followerCmd.PersistentFlags().Duration("replication.lease-interval", 15*time.Second, "Interval in which the workers re-new their table leases.")
	followerCmd.PersistentFlags().Duration("replication.log-rpc-timeout", 1*time.Minute, "The log RPC timeout.")
	followerCmd.PersistentFlags().Duration("replication.snapshot-rpc-timeout", 1*time.Hour, "The snapshot RPC timeout.")
	followerCmd.PersistentFlags().Uint64("replication.max-recv-message-size-bytes", 8*1024*1024, "The maximum size of single replication message allowed to receive.")
	followerCmd.PersistentFlags().Uint64("replication.max-recovery-in-flight", 1, "The maximum number of recovery goroutines allowed to run in this instance.")
	followerCmd.PersistentFlags().Uint64("replication.max-snapshot-recv-bytes-per-second", 0, "Maximum bytes per second received by the snapshot API client, default value 0 means unlimited.")
}

var followerCmd = &cobra.Command{
	Use:   "follower",
	Short: "Start Armada in follower mode.",
	RunE:  follower,
	PreRunE: func(cmd *cobra.Command, args []string) error {
		initConfig(cmd.PersistentFlags())
		return validateFollowerConfig()
	},
	DisableAutoGenTag: true,
}

func validateFollowerConfig() error {
	if !viper.IsSet("replication.leader-address") {
		return errors.New("leader address must be set")
	}
	if !viper.IsSet("raft.address") {
		return errors.New("raft address must be set")
	}
	return nil
}

func follower(_ *cobra.Command, _ []string) error {
	logger, log, shutdown, err := setupCommonEnvironment()
	if err != nil {
		return err
	}
	defer func() {
		_ = logger.Sync()
	}()

	engineLog := logger.Named("engine")

	// Create notification queue for follower mode
	nQueue := storage.NewNotificationQueue()
	go nQueue.Run()
	defer func() {
		_ = nQueue.Close()
	}()

	// Create and start the engine with notification queue
	config, err := createEngineConfig(engineLog, nQueue.Notify)
	if err != nil {
		return err
	}

	engine, err := startEngine(config)
	if err != nil {
		return err
	}
	defer engine.Close()

	// Replication
	conn, err := createReplicationConn(logger)
	if err != nil {
		return fmt.Errorf("cannot create replication conn: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()
	{
		d := replication.NewManager(engine, nQueue, conn, replication.Config{
			ReconcileInterval: viper.GetDuration("replication.reconcile-interval"),
			Workers: replication.WorkerConfig{
				PollInterval:        viper.GetDuration("replication.poll-interval"),
				LeaseInterval:       viper.GetDuration("replication.lease-interval"),
				LogRPCTimeout:       viper.GetDuration("replication.log-rpc-timeout"),
				SnapshotRPCTimeout:  viper.GetDuration("replication.snapshot-rpc-timeout"),
				MaxRecoveryInFlight: int64(viper.GetUint64("replication.max-recovery-in-flight")),
				MaxSnapshotRecv:     viper.GetUint64("replication.max-snapshot-recv-bytes-per-second"),
			},
		})
		prometheus.MustRegister(d)
		d.Start()
		defer d.Close()
	}

	// Start servers
	{
		{
			// Create regatta API server
			// Create server
			regatta, err := createAPIServer(logger.Named("server.api"), func(r grpc.ServiceRegistrar) {
				regattapb.RegisterKVServer(r, regattaserver.NewForwardingKVServer(engine, regattapb.NewKVClient(conn), nQueue))
				regattapb.RegisterClusterServer(r, &regattaserver.ClusterServer{
					Cluster: engine,
					Config:  viperConfigReader,
				})
				if viper.GetBool("maintenance.enabled") {
					regattapb.RegisterMaintenanceServer(r, &regattaserver.ResetServer{Tables: engine, AuthFunc: authFunc(viper.GetString("maintenance.token"))})
				}
				if viper.GetBool("tables.enabled") {
					regattapb.RegisterTablesServer(r, &regattaserver.ReadonlyTablesServer{TablesServer: regattaserver.TablesServer{Tables: engine, AuthFunc: authFunc(viper.GetString("tables.token"))}})
				}

				// Register metrics server for Prometheus metrics via gRPC
				metricsServer := regattaserver.NewMetricsServer(nil) // Using default registry
				regattapb.RegisterMetricsServer(r, metricsServer)
			})
			if err != nil {
				return fmt.Errorf("failed to create API server: %w", err)
			}

			// Start server
			go func() {
				if err := regatta.Serve(); err != nil {
					log.Errorf("grpc listenAndServe failed: %v", err)
				}
			}()
			defer regatta.Shutdown()
		}

		// Create REST server
		hs := setupRESTServer(log)
		defer hs.Shutdown()
	}

	// Wait for shutdown signal
	waitForShutdown(shutdown, log)
	return nil
}

func createReplicationConn(log *zap.Logger) (*grpc.ClientConn, error) {
	addr, secure, net := resolveURL(viper.GetString("replication.leader-address"))
	var creds grpc.DialOption
	if secure {
		ti := security.TLSInfo{
			CertFile:           viper.GetString("replication.cert-filename"),
			KeyFile:            viper.GetString("replication.key-filename"),
			TrustedCAFile:      viper.GetString("replication.ca-filename"),
			InsecureSkipVerify: viper.GetBool("replication.insecure-skip-verify"),
			ServerName:         viper.GetString("replication.server-name"),
			Logger:             log.Named("replication.cert").Sugar(),
		}
		cfg, err := ti.ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot build tls config: %w", err)
		}
		creds = grpc.WithTransportCredentials(credentials.NewTLS(cfg))
	} else {
		creds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	switch net {
	case "unix", "unixs":
		addr = fmt.Sprintf("unix://%s", addr)
	default:
		addr = fmt.Sprintf("dns:%s", addr)
	}

	replConn, err := grpc.NewClient(addr, creds,
		grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"round_robin":{}}]}`),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                viper.GetDuration("replication.keepalive-time"),
			Timeout:             viper.GetDuration("replication.keepalive-timeout"),
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(viper.GetUint64("replication.max-recv-message-size-bytes")))),
	)
	if err != nil {
		return nil, err
	}
	return replConn, nil
}
