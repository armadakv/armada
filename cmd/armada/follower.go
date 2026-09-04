// Copyright JAMF Software, LLC

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/armadaserver"
	"github.com/armadakv/armada/replication"
	"github.com/armadakv/armada/security"
	"github.com/armadakv/armada/storage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

var followerCmd = &cli.Command{
	Name:  "follower",
	Usage: "Start Armada in follower mode.",
	Flags: mergeFlags(
		rootFlags, apiFlags, restFlags, raftFlags, memberlistFlags,
		storageFlags, maintenanceFlags, tablesFlags, experimentalFlags,
		sharedStoreFlags,
		[]cli.Flag{
			&cli.StringFlag{Name: "replication.leader-address", Value: "localhost:8444", Usage: "Address of the leader replication API to connect to."},
			&cli.DurationFlag{Name: "replication.keepalive-time", Value: 60 * 1000 * 1000 * 1000, Usage: "After a duration of this time if the replication client doesn't see any activity it pings the server to see if the transport is still alive. If set below 10s, a minimum value of 10s will be used instead."},
			&cli.DurationFlag{Name: "replication.keepalive-timeout", Value: 10 * 1000 * 1000 * 1000, Usage: "After having pinged for keepalive check, the replication client waits for a duration of Timeout and if no activity is seen even after that the connection is closed."},
			&cli.StringFlag{Name: "replication.cert-filename", Value: "hack/replication/client.crt", Usage: "Path to the client certificate."},
			&cli.StringFlag{Name: "replication.key-filename", Value: "hack/replication/client.key", Usage: "Path to the client private key file."},
			&cli.StringFlag{Name: "replication.ca-filename", Value: "hack/replication/ca.crt", Usage: "Path to the client CA cert file. The CA file is used to verify server authority."},
			&cli.BoolFlag{Name: "replication.insecure-skip-verify", Value: false, Usage: "InsecureSkipVerify controls whether a client verifies the server's certificate chain and host name. If InsecureSkipVerify is true, crypto/tls accepts any certificate presented by the server and any host name in that certificate."},
			&cli.StringFlag{Name: "replication.server-name", Value: "", Usage: "ServerName ensures the cert matches the given host in case of discovery/virtual hosting."},
			&cli.DurationFlag{Name: "replication.poll-interval", Value: 1 * 1000 * 1000 * 1000, Usage: "Replication interval in seconds, the leader poll time."},
			&cli.DurationFlag{Name: "replication.reconcile-interval", Value: 30 * 1000 * 1000 * 1000, Usage: "Replication interval of tables reconciliation (workers startup/shutdown)."},
			&cli.DurationFlag{Name: "replication.lease-interval", Value: 15 * 1000 * 1000 * 1000, Usage: "Interval in which the workers re-new their table leases."},
			&cli.DurationFlag{Name: "replication.log-rpc-timeout", Value: 60 * 1000 * 1000 * 1000, Usage: "The log RPC timeout."},
			&cli.DurationFlag{Name: "replication.snapshot-rpc-timeout", Value: 3600 * 1000 * 1000 * 1000, Usage: "The snapshot RPC timeout."},
			&cli.Uint64Flag{Name: "replication.max-recv-message-size-bytes", Value: 8 * 1024 * 1024, Usage: "The maximum size of single replication message allowed to receive."},
			&cli.Uint64Flag{Name: "replication.max-recovery-in-flight", Value: 1, Usage: "The maximum number of recovery goroutines allowed to run in this instance."},
			&cli.StringFlag{Name: "replication.snapshot-source", Value: "auto", Usage: "Snapshot object source: auto, direct, or proxy. direct uses shared-store backend from follower config; proxy uses leader HTTP endpoint."},
		},
	),
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if err := initConfig(c); err != nil {
			return context.TODO(), err
		}
		return context.Background(), validateFollowerConfig()
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		return follower()
	},
}

func validateFollowerConfig() error {
	if !k.Exists("replication.leader-address") {
		return errors.New("leader address must be set")
	}
	if !k.Exists("raft.address") {
		return errors.New("raft address must be set")
	}
	return nil
}

func follower() error {
	logger, log, shutdown := setupCommonEnvironment()
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
		snapshotGetter, snapshotQuery, err := createSnapshotAccess(logger)
		if err != nil {
			return err
		}
		d := replication.NewManager(engine, nQueue, conn, snapshotGetter, snapshotQuery, replication.Config{
			ReconcileInterval: k.Duration("replication.reconcile-interval"),
			Workers: replication.WorkerConfig{
				PollInterval:        k.Duration("replication.poll-interval"),
				LeaseInterval:       k.Duration("replication.lease-interval"),
				LogRPCTimeout:       k.Duration("replication.log-rpc-timeout"),
				SnapshotRPCTimeout:  k.Duration("replication.snapshot-rpc-timeout"),
				MaxRecoveryInFlight: int64(uint64(k.Int64("replication.max-recovery-in-flight"))),
			},
		})
		prometheus.MustRegister(d)
		d.Start()
		defer d.Close()
	}

	// Start servers
	{
		{
			// Create API server
			// Create server
			regatta, err := createAPIServer(logger.Named("server.api"), func(r grpc.ServiceRegistrar) {
				armadapb.RegisterKVServer(r, armadaserver.NewForwardingKVServer(engine, armadapb.NewKVClient(conn), nQueue))
				armadapb.RegisterClusterServer(r, &armadaserver.ClusterServer{
					Cluster: engine,
					Config:  koanfConfigReader,
				})
				if k.Bool("maintenance.enabled") {
					armadapb.RegisterMaintenanceServer(r, &armadaserver.ResetServer{Tables: engine, AuthFunc: authFunc(k.String("maintenance.token"))})
				}
				if k.Bool("tables.enabled") {
					armadapb.RegisterTablesServer(r, &armadaserver.ReadonlyTablesServer{Tables: engine, AuthFunc: authFunc(k.String("tables.token"))})
				}

				// Register metrics server for Prometheus metrics via gRPC
				metricsServer := armadaserver.NewMetricsServer(nil) // Using default registry
				armadapb.RegisterMetricsServer(r, metricsServer)
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
	addr, secure, net := resolveURL(k.String("replication.leader-address"))
	var creds grpc.DialOption
	if secure {
		ti := security.TLSInfo{
			CertFile:           k.String("replication.cert-filename"),
			KeyFile:            k.String("replication.key-filename"),
			TrustedCAFile:      k.String("replication.ca-filename"),
			InsecureSkipVerify: k.Bool("replication.insecure-skip-verify"),
			ServerName:         k.String("replication.server-name"),
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
			Time:                k.Duration("replication.keepalive-time"),
			Timeout:             k.Duration("replication.keepalive-timeout"),
			PermitWithoutStream: true,
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(uint64(k.Int64("replication.max-recv-message-size-bytes"))))),
	)
	if err != nil {
		return nil, err
	}
	return replConn, nil
}

func createSnapshotAccess(logger *zap.Logger) (replication.SnapshotObjectGetter, replication.SnapshotQueryResolver, error) {
	source := k.String("replication.snapshot-source")
	backend := k.String("shared-store.backend")
	if source == "direct" || (source == "auto" && backend != "" && backend != "none") {
		bkt, err := newBucketFromConfig(context.Background(), BucketConfig{
			Backend:        backend,
			Directory:      k.String("shared-store.filesystem.directory"),
			S3Bucket:       k.String("shared-store.s3.bucket"),
			GCSBucket:      k.String("shared-store.gcs.bucket"),
			AzureContainer: k.String("shared-store.azure.container"),
			AzureAccount:   k.String("shared-store.azure.account"),
			AzureKey:       k.String("shared-store.azure.key"),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create shared-store bucket for direct snapshot mode: %w", err)
		}
		if bkt == nil {
			return nil, nil, fmt.Errorf("snapshot source %q requires shared-store backend configuration", source)
		}
		return replication.NewBucketSnapshotObjectGetter(bkt), replication.NewBucketSnapshotQueryResolver(bkt), nil
	}
	if source != "proxy" && source != "auto" {
		return nil, nil, fmt.Errorf("invalid replication.snapshot-source %q: must be one of auto,direct,proxy", source)
	}

	addr, secure, net := resolveURL(k.String("replication.leader-address"))
	httpClient, err := replication.NewHTTPClient(
		logger.Named("replication.http").Sugar(),
		addr,
		k.String("replication.cert-filename"),
		k.String("replication.key-filename"),
		k.String("replication.ca-filename"),
		k.Bool("replication.insecure-skip-verify"),
		k.String("replication.server-name"),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create replication http client: %w", err)
	}

	scheme := "http"
	if secure {
		scheme = "https"
	}
	if net == "unix" || net == "unixs" {
		scheme = "http"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, addr)
	return replication.NewHTTPSnapshotObjectGetter(httpClient, baseURL), nil, nil
}
