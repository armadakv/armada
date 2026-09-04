// Copyright JAMF Software, LLC

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/armadaserver"
	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/armada/security"
	"github.com/armadakv/armada/storage"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/objfs"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
)

var leaderCmd = &cli.Command{
	Name:  "leader",
	Usage: "Start Armada in leader mode.",
	Flags: mergeFlags(
		rootFlags, apiFlags, restFlags, raftFlags, memberlistFlags,
		storageFlags, maintenanceFlags, tablesFlags, experimentalFlags,
		sharedStoreFlags,
		[]cli.Flag{
			&cli.StringSliceFlag{Name: "tables.names", Usage: "Create Armada tables with given names."},
			&cli.StringSliceFlag{Name: "tables.delete", Usage: "Delete Armada tables with given names."},
			&cli.BoolFlag{Name: "replication.enabled", Value: true, Usage: "Whether replication API is enabled."},
			&cli.Uint64Flag{Name: "replication.max-send-message-size-bytes", Value: armadaserver.DefaultMaxGRPCSize, Usage: "The target maximum size of single replication message allowed to send."},
			&cli.StringFlag{Name: "replication.address", Value: "http://0.0.0.0:8444", Usage: "Replication API server address. The address the server listens on."},
			&cli.StringFlag{Name: "replication.cert-filename", Value: "", Usage: "Path to the API server certificate."},
			&cli.StringFlag{Name: "replication.key-filename", Value: "", Usage: "Path to the API server private key file."},
			&cli.StringFlag{Name: "replication.ca-filename", Value: "", Usage: "Path to the API server CA cert file."},
			&cli.BoolFlag{Name: "replication.client-cert-auth", Value: false, Usage: "Replication server client certificate auth enabled. If set to true the `replication.ca-filename` should be provided as well."},
		},
	),
	Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
		if err := initConfig(c); err != nil {
			return context.TODO(), err
		}
		return context.Background(), validateLeaderConfig()
	},
	Action: func(ctx context.Context, c *cli.Command) error {
		return leader()
	},
}

func validateLeaderConfig() error {
	if !k.Exists("raft.address") {
		return errors.New("raft address must be set")
	}
	return nil
}

func leader() error {
	logger, log, shutdown := setupCommonEnvironment()
	defer func() {
		_ = logger.Sync()
	}()

	engineLog := logger.Named("engine")

	// Create the engine (but do not start it yet) so that SnapshotNotifier can
	// be wired before any event-dispatch goroutines are running.
	config, err := createEngineConfig(engineLog, nil)
	if err != nil {
		return err
	}

	engine, err := storage.New(config)
	if err != nil {
		return fmt.Errorf("failed to create engine: %w", err)
	}
	prometheus.MustRegister(engine)
	defer engine.Close()

	// Start snapshot exporter and GC worker when a non-none backend is configured.
	// This must happen before engine.Start() to avoid a data race on SnapshotNotifier
	// and to ensure no compaction notifications are dropped.
	snapshotCtx, cancelSnapshot := context.WithCancel(context.Background())
	defer cancelSnapshot()
	var sharedStoreBucket objfs.Bucket
	if backend := k.String("shared-store.backend"); backend != "" && backend != "none" {
		bkt, err := newBucketFromConfig(snapshotCtx, BucketConfig{
			Backend:        backend,
			Directory:      k.String("shared-store.filesystem.directory"),
			S3Bucket:       k.String("shared-store.s3.bucket"),
			GCSBucket:      k.String("shared-store.gcs.bucket"),
			AzureContainer: k.String("shared-store.azure.container"),
			AzureAccount:   k.String("shared-store.azure.account"),
			AzureKey:       k.String("shared-store.azure.key"),
		})
		if err != nil {
			return fmt.Errorf("snapshot-store: %w", err)
		}
		sharedStoreBucket = bkt
		// Use the Raft address as a human-readable, cluster-unique node identifier
		// in snapshot meta files. There is no separate node-id config option.
		nodeAddress := k.String("raft.address")

		exp := store.NewSnapshotExporter(
			store.NewEngineTableService(engine),
			replicationExporterConfig(nodeAddress, bkt),
			logger.Sugar(),
		)
		engine.SnapshotNotifier = exp
		go exp.Run(snapshotCtx)

		gc := store.NewGCWorker(sharedStoreGCConfig(bkt), logger.Sugar())
		go gc.Run(snapshotCtx)
	}

	if err := engine.Start(); err != nil {
		return fmt.Errorf("failed to start engine: %w", err)
	}

	go func() {
		waitForEngine(context.Background(), engine, log)

		// Create and delete tables as specified in configuration.
		tNames := k.Strings("tables.names")
		for _, table := range tNames {
			log.Debugf("creating table %s", table)
			if _, err := engine.CreateTable(table); err != nil {
				if errors.Is(err, serrors.ErrTableExists) {
					log.Infof("table %s already exist, skipping creation", table)
				} else {
					log.Errorf("failed to create table %s: %v", table, err)
				}
			}
		}
		dNames := k.Strings("tables.delete")
		for _, table := range dNames {
			log.Debugf("deleting table %s", table)
			err := engine.DeleteTable(table)
			if err != nil {
				log.Errorf("failed to delete table %s: %v", table, err)
			}
		}
	}()

	{
		// Create API server
		{
			regatta, err := createAPIServer(logger.Named("server.api"), func(r grpc.ServiceRegistrar) {
				armadapb.RegisterKVServer(r, &armadaserver.KVServer{
					Storage: engine,
				})
				armadapb.RegisterClusterServer(r, &armadaserver.ClusterServer{
					Cluster: engine,
					Config:  koanfConfigReader,
				})
				if k.Bool("tables.enabled") {
					armadapb.RegisterTablesServer(r, &armadaserver.TablesServer{Tables: engine, AuthFunc: authFunc(k.String("tables.token"))})
				}
				if k.Bool("maintenance.enabled") {
					armadapb.RegisterMaintenanceServer(r, &armadaserver.BackupServer{Tables: engine, AuthFunc: authFunc(k.String("maintenance.token"))})
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

		if k.Bool("replication.enabled") {
			// Load replication API certificate
			replication, err := createReplicationServer(logger.Named("server.replication"), func(r grpc.ServiceRegistrar) {
				ls := armadaserver.NewLogServer(
					engine,
					engine.LogReader,
					logger,
					uint64(k.Int64("replication.max-send-message-size-bytes")),
				)
				armadapb.RegisterMetadataServer(r, &armadaserver.MetadataServer{Tables: engine})
				armadapb.RegisterSnapshotServer(r, &armadaserver.SnapshotServer{Tables: engine, SnapshotStore: sharedStoreBucket})
				armadapb.RegisterKVServer(r, &armadaserver.KVServer{Storage: engine})
				armadapb.RegisterLogServer(r, ls)
			}, func(mux *http.ServeMux) {
				mux.Handle("/", store.NewSnapshotHTTPHandler(sharedStoreBucket, engine, logger.Named("snapshot-http").Sugar()))
			})
			if err != nil {
				return fmt.Errorf("failed to create Replication server: %w", err)
			}

			// Start server
			go func() {
				if err := replication.Serve(); err != nil {
					log.Errorf("grpc listenAndServe failed: %v", err)
				}
			}()
			defer replication.Shutdown()
		}

		// Create REST server
		hs := setupRESTServer(log)
		defer hs.Shutdown()
	}

	// Wait for shutdown signal
	waitForShutdown(shutdown, log)
	return nil
}

type replicationServer interface {
	Serve() error
	Shutdown()
}

func createReplicationServer(log *zap.Logger, reg func(r grpc.ServiceRegistrar), httpReg func(mux *http.ServeMux)) (replicationServer, error) {
	addr, secure, nw := resolveURL(k.String("replication.address"))
	lopts := []logging.Option{logging.WithLogOnEvents(logging.FinishCall), logging.WithLevels(codeToLevel)}
	opts := []grpc.ServerOption{
		grpc.ChainStreamInterceptor(
			grpcmetrics.StreamServerInterceptor(),
			logging.StreamServerInterceptor(interceptorLogger(log), lopts...),
		),
		grpc.ChainUnaryInterceptor(
			grpcmetrics.UnaryServerInterceptor(),
			logging.UnaryServerInterceptor(interceptorLogger(log), lopts...),
		),
	}
	var tlsCfg *tls.Config
	if secure {
		ti := security.TLSInfo{
			CertFile:        k.String("replication.cert-filename"),
			KeyFile:         k.String("replication.key-filename"),
			TrustedCAFile:   k.String("replication.ca-filename"),
			ClientCertAuth:  k.Bool("replication.client-cert-auth"),
			AllowedCN:       k.String("replication.allowed-cn"),
			AllowedHostname: k.String("replication.allowed-hostname"),
			Logger:          log.Named("cert").Sugar(),
		}
		cfg, err := ti.ServerConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot build tls config: %w", err)
		}
		tlsCfg = cfg
		if httpReg == nil {
			opts = append(opts, grpc.Creds(credentials.NewTLS(cfg)))
		}
	}
	// Create replication server
	l, err := net.Listen(nw, addr)
	if err != nil {
		return nil, err
	}
	server := armadaserver.NewServer(l, log.Sugar(), opts...)
	reg(server)
	grpcmetrics.InitializeMetrics(server.Server)
	if httpReg == nil {
		return server, nil
	}

	mux := http.NewServeMux()
	httpReg(mux)
	listener := l
	if secure {
		listener = tls.NewListener(l, tlsCfg)
	}
	return armadaserver.NewMixedServer(listener, server, mux, log.Sugar()), nil
}

func codeToLevel(code codes.Code) logging.Level {
	switch code {
	case codes.OK, codes.NotFound, codes.Canceled, codes.AlreadyExists, codes.InvalidArgument, codes.Unauthenticated:
		return logging.LevelDebug
	case codes.DeadlineExceeded, codes.PermissionDenied, codes.ResourceExhausted, codes.FailedPrecondition, codes.Aborted,
		codes.OutOfRange, codes.Unavailable:
		return logging.LevelWarn
	case codes.Unknown, codes.Unimplemented, codes.Internal, codes.DataLoss:
		return logging.LevelError
	default:
		return logging.LevelError
	}
}

func interceptorLogger(l *zap.Logger) logging.Logger {
	return logging.LoggerFunc(func(ctx context.Context, lvl logging.Level, msg string, fields ...any) {
		f := make([]zap.Field, 0, len(fields)/2)

		for i := 0; i < len(fields); i += 2 {
			key := fields[i]
			value := fields[i+1]

			switch v := value.(type) {
			case string:
				f = append(f, zap.String(key.(string), v))
			case int:
				f = append(f, zap.Int(key.(string), v))
			case bool:
				f = append(f, zap.Bool(key.(string), v))
			default:
				f = append(f, zap.Any(key.(string), v))
			}
		}

		logger := l.WithOptions(zap.AddCallerSkip(1)).With(f...)

		switch lvl {
		case logging.LevelDebug:
			logger.Debug(msg)
		case logging.LevelInfo:
			logger.Info(msg)
		case logging.LevelWarn:
			logger.Warn(msg)
		case logging.LevelError:
			logger.Error(msg)
		default:
			panic(fmt.Sprintf("unknown level %v", lvl))
		}
	})
}
