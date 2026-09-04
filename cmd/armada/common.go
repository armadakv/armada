// Copyright Armada Contributors

// Command armada is the Armada server binary.
// It defines the root command and the "leader", "follower", "version", and
// "docs" sub-commands, wiring together configuration, logging, and server startup.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/armadakv/armada/armadaserver"
	rl "github.com/armadakv/armada/log"
	dbl "github.com/armadakv/armada/raft/logger"
	"github.com/armadakv/armada/security"
	"github.com/armadakv/armada/storage/table"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/auth"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli/v3"

	"go.uber.org/zap"
	"golang.org/x/net/netutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

var k = koanf.New(".")

func initConfig(c *cli.Command) error {
	// Load configuration into the koanf instance with flat, dot-separated keys
	// (e.g. "raft.address"). Sources are loaded from lowest to highest priority:
	// flag defaults < environment variables < explicitly set command-line flags.
	defaults := make(map[string]any)
	explicit := make(map[string]any)
	for _, f := range c.Flags {
		name := f.Names()[0]
		if c.IsSet(name) {
			explicit[name] = c.Value(name)
		} else {
			defaults[name] = c.Value(name)
		}
	}

	// 1. Flag defaults (lowest priority).
	if err := k.Load(confmap.Provider(defaults, "."), nil); err != nil {
		return err
	}

	// 2. Config file, if one was specified via --config. The format is detected
	// from the file extension. Values here override flag defaults but are
	// overridden by environment variables and explicitly set flags.
	if path := c.String("config"); path != "" {
		parser, err := configParser(path)
		if err != nil {
			return err
		}
		if err := k.Load(file.Provider(path), parser); err != nil {
			return fmt.Errorf("failed to load config file %q: %w", path, err)
		}
	}

	// 3. Environment variables prefixed with ARMADA_. The prefix is stripped and
	// remaining underscores are converted to dots so ARMADA_RAFT_ADDRESS maps to
	// the "raft.address" key.
	if err := k.Load(env.Provider("ARMADA_", ".", func(s string) string {
		return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, "ARMADA_")), "_", ".")
	}), nil); err != nil {
		return err
	}

	// 4. Explicitly set command-line flags (highest priority).
	if err := k.Load(confmap.Provider(explicit, "."), nil); err != nil {
		return err
	}

	return nil
}

// configParser returns the koanf parser matching the config file's extension.
func configParser(path string) (koanf.Parser, error) {
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".yaml", ".yml":
		return yaml.Parser(), nil
	case ".json":
		return json.Parser(), nil
	case ".toml":
		return toml.Parser(), nil
	default:
		return nil, fmt.Errorf("unsupported config file format %q (supported: .yaml, .yml, .json, .toml)", ext)
	}
}

func mergeFlags(flagSlices ...[]cli.Flag) []cli.Flag {
	var merged []cli.Flag
	for _, fs := range flagSlices {
		merged = append(merged, fs...)
	}
	return merged
}

var (
	apiBuckets  = []float64{.0001, .001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 30}
	grpcmetrics = grpcprom.NewServerMetrics(grpcprom.WithServerHandlingTimeHistogram(grpcprom.WithHistogramBuckets(apiBuckets)))
)

func init() {
	prometheus.DefaultRegisterer.MustRegister(grpcmetrics)
}

func createAPIServer(log *zap.Logger, reg func(grpc.ServiceRegistrar)) (*armadaserver.Server, error) {
	addr, secure, nw := resolveURL(k.String("api.address"))
	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: 60 * time.Second}),
		grpc.ChainStreamInterceptor(
			auth.StreamServerInterceptor(defaultAuthFunc),
			grpcmetrics.StreamServerInterceptor(),
		),
		grpc.ChainUnaryInterceptor(
			auth.UnaryServerInterceptor(defaultAuthFunc),
			grpcmetrics.UnaryServerInterceptor(),
		),
		grpc.MaxConcurrentStreams(uint32(k.Int("api.max-concurrent-streams"))),
	}
	workers := k.Int("api.stream-workers")
	if workers > 0 {
		opts = append(opts, grpc.NumStreamWorkers(uint32(workers)))
	} else if workers == 0 {
		opts = append(opts, grpc.NumStreamWorkers(uint32(runtime.NumCPU()+1)))
	}
	if secure {
		ti := security.TLSInfo{
			CertFile:        k.String("api.cert-filename"),
			KeyFile:         k.String("api.key-filename"),
			TrustedCAFile:   k.String("api.ca-filename"),
			ClientCertAuth:  k.Bool("api.client-cert-auth"),
			AllowedCN:       k.String("api.allowed-cn"),
			AllowedHostname: k.String("api.allowed-hostname"),
			Logger:          log.Named("cert").Sugar(),
		}
		cfg, err := ti.ServerConfig()
		if err != nil {
			return nil, fmt.Errorf("cannot build tls config: %w", err)
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(cfg)))
	}
	l, err := net.Listen(nw, addr)
	if err != nil {
		return nil, err
	}
	if limit := uint32(k.Int("api.max-concurrent-connections")); limit > 0 {
		l = netutil.LimitListener(l, int(limit))
	}
	server := armadaserver.NewServer(l, log.Sugar(), opts...)
	reg(server)
	grpcmetrics.InitializeMetrics(server.Server)
	return server, nil
}

func resolveURL(urlStr string) (addr string, secure bool, network string) {
	u, err := url.Parse(urlStr)
	if err != nil {
		log.Panicf("cannot parse address: %v", err)
	}
	addr = u.Host
	network = "tcp"
	if u.Scheme == "unix" || u.Scheme == "unixs" {
		addr = u.Host + u.Path
		network = "unix"
	}
	secure = u.Scheme == "https" || u.Scheme == "unixs"
	return addr, secure, network
}

func toRecoveryType(str string) table.SnapshotRecoveryType {
	switch str {
	case "snapshot":
		return table.RecoveryTypeSnapshot
	case "checkpoint":
		return table.RecoveryTypeCheckpoint
	default:
		if runtime.GOOS == "windows" {
			return table.RecoveryTypeSnapshot
		}
		return table.RecoveryTypeCheckpoint
	}
}

func authFunc(token string) func(ctx context.Context) (context.Context, error) {
	if token == "" {
		return func(ctx context.Context) (context.Context, error) {
			return ctx, nil
		}
	}
	return func(ctx context.Context) (context.Context, error) {
		t, err := auth.AuthFromMD(ctx, "bearer")
		if err != nil {
			return ctx, err
		}
		if token != t {
			return ctx, status.Errorf(codes.Unauthenticated, "Invalid token")
		}
		return ctx, nil
	}
}

var defaultAuthFunc = authFunc("")

type tokenCredentials string

// nolint:unparam
func (t tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	if t != "" {
		return map[string]string{"authorization": "Bearer " + string(t)}, nil
	}
	return nil, nil
}

func (tokenCredentials) RequireTransportSecurity() bool {
	return true
}

// parseInitialMembersList converts an ordered list of raft addresses into the
// (nodeID, membersMap) pair required by the storage engine. The position in
// the list (1-based) is the replica ID. The node's own replica ID is derived
// by finding raftAddress in the list.
//
// The list must always be provided — it is the authoritative source of the
// node's replica ID and cannot be inferred from disk state.
func parseInitialMembersList(members []string, raftAddress string) (nodeID uint64, membersMap map[uint64]string, err error) {
	members = filterNonEmpty(members)
	if len(members) == 0 {
		return 0, nil, fmt.Errorf("--raft.initial-members must not be empty: the ordered list is required to determine the local replica ID")
	}
	membersMap = make(map[uint64]string, len(members))
	for i, addr := range members {
		replicaID := uint64(i + 1)
		membersMap[replicaID] = addr
		if addr == raftAddress {
			nodeID = replicaID
		}
	}
	if nodeID == 0 {
		return 0, nil, fmt.Errorf("raft address %q not found in initial-members list %v", raftAddress, members)
	}
	return nodeID, membersMap, nil
}

var dbLoggerOnce sync.Once

func setupDragonboatLogger(logger *zap.Logger) {
	dbLoggerOnce.Do(func() {
		dbl.SetLoggerFactory(rl.LoggerFactory(logger))
		dbl.GetLogger("raft").SetLevel(dbl.WARNING)
		dbl.GetLogger("rsm").SetLevel(dbl.WARNING)
		dbl.GetLogger("transport").SetLevel(dbl.ERROR)
		dbl.GetLogger("dragonboat").SetLevel(dbl.WARNING)
		dbl.GetLogger("logdb").SetLevel(dbl.INFO)
		dbl.GetLogger("tan").SetLevel(dbl.INFO)
		dbl.GetLogger("settings").SetLevel(dbl.INFO)
	})
}

var secretConfigs = []string{
	"maintenance.token",
	"tables.token",
}

func koanfConfigReader() map[string]any {
	res := make(map[string]any)
	for _, key := range k.Keys() {
		if slices.Contains(secretConfigs, key) {
			res[key] = "**********"
		} else {
			res[key] = k.Get(key)
		}
	}
	return res
}

// filterNonEmpty returns a new slice with empty strings removed.
func filterNonEmpty(ss []string) []string {
	out := ss[:0:0]
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
