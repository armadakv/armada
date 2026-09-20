package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dcontainer "github.com/moby/moby/api/types/container"
	dnetwork "github.com/moby/moby/api/types/network"
	dclient "github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Ports inside every container. Each node gets its own address on the harness
// network, so the internal port numbers can be identical on every node; only
// the host-side published ports have to differ.
const (
	portRaft        = 5000 // QUIC/UDP, shared by raft and gossip; internal only
	portAPI         = 5001 // gRPC KV/Tables/Cluster API
	portReplication = 5002 // leader-only replication gRPC + snapshot HTTP
	portREST        = 5010 // /metrics and /healthz
)

// Role distinguishes the two clusters the harness runs.
type Role string

const (
	RoleLeader   Role = "leader"
	RoleFollower Role = "follower"
)

// SnapshotSource selects how the follower cluster fetches snapshot artefacts.
type SnapshotSource string

const (
	// SourceProxy pulls artefacts over the leader's replication HTTP endpoint.
	// This is the credential-free path and the one that exercises
	// Range-resumable downloads through the leader.
	SourceProxy SnapshotSource = "proxy"
	// SourceDirect hands the follower the shared bucket so it reads artefacts
	// itself, bypassing the leader.
	SourceDirect SnapshotSource = "direct"
)

// Node is one armada process in its own container.
type Node struct {
	Name string
	Role Role
	// IP is the node's static address on the harness network. It is also the
	// node's raft address, and therefore its identity: the replica ID is the
	// 1-based position of this address in --raft.initial-members.
	IP        string
	ReplicaID uint64
	// APIAddr and RESTAddr are host-side "127.0.0.1:port" endpoints. They are
	// fixed rather than ephemeral so that they survive a container restart and
	// so that a human can point arq/arctl at a paused harness.
	APIAddr  string
	RESTAddr string

	env     *Env
	cmd     []string
	dataDir string
	ctr     testcontainers.Container

	mu   sync.Mutex
	conn *grpc.ClientConn
}

// Group is one cluster: an ordered set of nodes whose position defines their
// replica IDs.
type Group struct {
	Role  Role
	Nodes []*Node

	env *Env
	// source only applies to follower groups.
	source SnapshotSource
}

// Env is the whole harness: two clusters, a shared store, and the Docker
// plumbing that holds them together.
type Env struct {
	t     *testing.T
	Table string
	// Root is the host directory holding every node's data, the shared store
	// and the saved logs. Under ARMADA_TEST_ROOT it survives the run.
	Root      string
	StoreDir  string
	LogDir    string
	CertsDir  string
	Image     string
	NetName   string
	Leaders   *Group
	Followers *Group

	cfg     Config
	net     *testcontainers.DockerNetwork
	docker  *testcontainers.DockerClient
	seeded  bool
	keepEnv bool
	// written is the exclusive upper bound of the key range written through
	// this environment.
	written int
}

// ── lifecycle ────────────────────────────────────────────────────────────────

// New brings up the leader and follower clusters and registers teardown.
//
// The leader cluster is fully ready (its table accepts proposals) when this
// returns; the follower cluster is only launched, because several scenarios
// care about what it does while it is still catching up.
func New(t *testing.T) *Env {
	t.Helper()
	cfg := LoadConfig(t)
	ctx := context.Background()

	root := cfg.Root
	if root == "" {
		root = t.TempDir()
	} else {
		// Every subtest owns an environment. Keep its durable state, shared
		// store, and logs under a distinct child so one destructive recovery
		// scenario cannot affect the next one or erase its diagnostics.
		root = filepath.Join(root, testRootName(t.Name()))
		if err := os.RemoveAll(root); err != nil {
			t.Fatalf("clear root %s: %v", root, err)
		}
	}
	repo, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}

	e := &Env{
		t:        t,
		Table:    cfg.Table,
		Root:     root,
		StoreDir: filepath.Join(root, "shared-store"),
		LogDir:   filepath.Join(root, "logs"),
		CertsDir: filepath.Join(repo, "hack", "replication"),
		cfg:      cfg,
		keepEnv:  cfg.Keep,
	}
	// The container runs as uid 65532 (distroless nonroot) while these
	// directories are created by the test user, so they have to be
	// world-writable for the bind mounts to be usable on Linux hosts.
	for _, d := range []string{e.StoreDir, e.LogDir} {
		mkdirAllShared(t, d)
	}

	t.Cleanup(setActive(t))
	e.Image = armadaImage(ctx, t)
	e.reservePorts()

	e.docker, err = testcontainers.NewDockerClientWithOpts(ctx)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	prefix, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil {
		t.Fatalf("invalid ARMADA_TEST_SUBNET %q: %v", cfg.Subnet, err)
	}
	e.net, err = tcnetwork.New(ctx, tcnetwork.WithIPAM(&dnetwork.IPAM{
		Driver: "default",
		Config: []dnetwork.IPAMConfig{{Subnet: prefix, Gateway: prefix.Addr().Next()}},
	}))
	if err != nil {
		t.Fatalf("create harness network %s: %v", cfg.Subnet, err)
	}
	e.NetName = e.net.Name

	t.Cleanup(func() { e.teardown() })

	e.Leaders = e.newGroup(RoleLeader, cfg.LeaderIPs, "")
	e.Followers = e.newGroup(RoleFollower, cfg.FollowerIPs, SourceProxy)

	e.Leaders.mustStart(ctx, t)
	e.awaitLeaderTable(ctx)
	e.Followers.mustStart(ctx, t)
	return e
}

// testRootName converts a Go test name into one safe path element. Go uses
// slashes to identify subtests, while a configured test root is a directory
// shared by all of them.
func testRootName(name string) string {
	return strings.NewReplacer("/", "-", "\\", "-").Replace(name)
}

func (e *Env) teardown() {
	ctx := context.Background()
	// Save logs unconditionally: a passing run's logs are the baseline you
	// compare the next failure against, and they are gone once the containers
	// are removed.
	for _, g := range []*Group{e.Leaders, e.Followers} {
		if g == nil {
			continue
		}
		for _, n := range g.Nodes {
			n.saveLogs(ctx)
			n.closeConn()
		}
	}
	if e.keepEnv {
		infof("ARMADA_TEST_KEEP set: leaving containers and %s in place", e.Root)
		infof("  leader api  %s", e.Leaders.Nodes[0].APIAddr)
		infof("  follower api %s", e.Followers.Nodes[0].APIAddr)
		return
	}
	for _, g := range []*Group{e.Followers, e.Leaders} {
		if g == nil {
			continue
		}
		for _, n := range g.Nodes {
			n.terminate(ctx)
		}
	}
	if e.net != nil {
		_ = e.net.Remove(ctx)
	}
	if e.docker != nil {
		_ = e.docker.Close()
	}
	if e.t.Failed() {
		infof("container logs saved under %s", e.LogDir)
	}
}

// reservePorts fails early and clearly when the fixed host ports are taken,
// rather than letting Docker fail mid-startup with a port-allocation error.
func (e *Env) reservePorts() {
	var busy []string
	for _, p := range e.cfg.allHostPorts() {
		l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err != nil {
			busy = append(busy, strconv.Itoa(p))
			continue
		}
		_ = l.Close()
	}
	if len(busy) > 0 {
		e.t.Fatalf("host ports already in use: %s (another harness running? or set ARMADA_TEST_PORT_BASE)",
			strings.Join(busy, ", "))
	}
}

// ── group construction ───────────────────────────────────────────────────────

func (e *Env) newGroup(role Role, ips []string, source SnapshotSource) *Group {
	g := &Group{Role: role, env: e, source: source}
	for i, ip := range ips {
		n := &Node{
			Name:      fmt.Sprintf("%s%d", role, i+1),
			Role:      role,
			IP:        ip,
			ReplicaID: uint64(i + 1),
			APIAddr:   net.JoinHostPort("127.0.0.1", strconv.Itoa(e.cfg.hostAPIPort(role, i))),
			RESTAddr:  net.JoinHostPort("127.0.0.1", strconv.Itoa(e.cfg.hostRESTPort(role, i))),
			env:       e,
			dataDir:   filepath.Join(e.Root, fmt.Sprintf("%s-%d", role, i+1)),
		}
		mkdirAllShared(e.t, filepath.Join(n.dataDir, "raft"))
		mkdirAllShared(e.t, filepath.Join(n.dataDir, "state-machine"))
		g.Nodes = append(g.Nodes, n)
	}
	g.refreshCmds()
	return g
}

// refreshCmds recomputes every node's argv. Called whenever something that
// feeds the command line changes, e.g. the follower snapshot source.
func (g *Group) refreshCmds() {
	peers := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		peers = append(peers, fmt.Sprintf("%s:%d", n.IP, portRaft))
	}
	initialMembers := strings.Join(peers, ",")
	for _, n := range g.Nodes {
		if g.Role == RoleLeader {
			n.cmd = g.env.leaderCmd(n, initialMembers)
		} else {
			n.cmd = g.env.followerCmd(n, initialMembers, g.source)
		}
	}
}

func (e *Env) commonCmd(n *Node, initialMembers string) []string {
	return []string{
		"--dev-mode",
		"--log-level=DEBUG",
		"--raft.node-host-dir=/data/raft",
		"--raft.state-machine-dir=/data/state-machine",
		fmt.Sprintf("--raft.address=%s:%d", n.IP, portRaft),
		"--raft.initial-members=" + initialMembers,
		fmt.Sprintf("--api.address=http://0.0.0.0:%d", portAPI),
		fmt.Sprintf("--api.advertise-address=http://%s:%d", n.IP, portAPI),
		fmt.Sprintf("--rest.address=http://0.0.0.0:%d", portREST),
	}
}

func (e *Env) leaderCmd(n *Node, initialMembers string) []string {
	cmd := append([]string{"leader"}, e.commonCmd(n, initialMembers)...)
	return append(cmd,
		// Small enough that the raft log compacts after a few hundred writes and
		// the MVCC GC horizon actually advances. That is what makes the leader
		// answer USE_SNAPSHOT, and it bounds which incremental artefacts are
		// still valid.
		fmt.Sprintf("--raft.snapshot-entries=%d", e.cfg.LeaderSnapshotEntries),
		fmt.Sprintf("--raft.compaction-overhead=%d", e.cfg.LeaderCompactionOverhead),
		"--api.max-concurrent-streams=1000",
		fmt.Sprintf("--replication.address=https://0.0.0.0:%d", portReplication),
		"--replication.ca-filename=/certs/ca.crt",
		"--replication.cert-filename=/certs/server.crt",
		"--replication.key-filename=/certs/server.key",
		"--shared-store.backend=filesystem",
		"--shared-store.filesystem.directory=/shared-store",
		// A full snapshot base has to exist quickly: incremental chains are
		// useless without one.
		"--shared-store.full-interval="+e.cfg.FullInterval.String(),
		fmt.Sprintf("--shared-store.incr-max-chain=%d", e.cfg.IncrMaxChain),
		"--shared-store.gc-interval=1m",
		"--shared-store.retention=30m",
	)
}

func (e *Env) followerCmd(n *Node, initialMembers string, source SnapshotSource) []string {
	cmd := append([]string{"follower"}, e.commonCmd(n, initialMembers)...)
	cmd = append(cmd,
		fmt.Sprintf("--raft.snapshot-entries=%d", e.cfg.FollowerSnapshotEntries),
		fmt.Sprintf("--raft.compaction-overhead=%d", e.cfg.FollowerCompactionOverhead),
		fmt.Sprintf("--replication.leader-address=https://%s:%d", e.Leaders.Nodes[0].IP, portReplication),
		// The fixture certificates are issued for localhost/127.0.0.1 only,
		// while inside the harness network the leader is reached by container
		// IP. Pinning the verified name keeps the real TLS handshake (and the
		// real chain verification) instead of disabling it.
		"--replication.server-name=localhost",
		"--replication.ca-filename=/certs/ca.crt",
		"--replication.cert-filename=/certs/client.crt",
		"--replication.key-filename=/certs/client.key",
		"--replication.poll-interval=500ms",
		"--replication.reconcile-interval=3s",
		"--replication.lease-interval=2s",
		"--replication.max-recovery-in-flight=1",
	)
	if source == SourceDirect {
		cmd = append(cmd,
			"--replication.snapshot-source=direct",
			"--shared-store.backend=filesystem",
			"--shared-store.filesystem.directory=/shared-store",
		)
	}
	return cmd
}

// ── container operations ─────────────────────────────────────────────────────

func (n *Node) request() testcontainers.ContainerRequest {
	e := n.env
	binds := []string{
		n.dataDir + ":/data",
		e.StoreDir + ":/shared-store",
		e.CertsDir + ":/certs:ro",
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	bindings := dnetwork.PortMap{
		tcpPort(portAPI):  {{HostIP: loopback, HostPort: portOf(n.APIAddr)}},
		tcpPort(portREST): {{HostIP: loopback, HostPort: portOf(n.RESTAddr)}},
	}
	ip := netip.MustParseAddr(n.IP)
	return testcontainers.ContainerRequest{
		Image:          e.Image,
		Name:           fmt.Sprintf("armada-it-%s-%s", sanitize(e.NetName), n.Name),
		Cmd:            n.cmd,
		Networks:       []string{e.NetName},
		NetworkAliases: map[string][]string{e.NetName: {n.Name}},
		ExposedPorts: []string{
			fmt.Sprintf("%d/tcp", portAPI),
			fmt.Sprintf("%d/tcp", portREST),
		},
		// Pin the hostname to the logical node name.
		//
		// Dragonboat records os.Hostname() in the NodeHost directory and
		// refuses to open it under a different one (ErrHostnameChanged,
		// raft/internal/server/environment.go) — a guard against a data
		// directory being moved between machines. Docker otherwise derives the
		// hostname from the container id, so recreating a container over a
		// surviving data directory looks exactly like that move and the node
		// dies on startup with "hostname changed".
		//
		// Restarting a container keeps its id, which is why this only bites
		// after Rebuild.
		ConfigModifier: func(cfg *dcontainer.Config) {
			cfg.Hostname = n.Name
		},
		HostConfigModifier: func(hc *dcontainer.HostConfig) {
			hc.Binds = binds
			hc.PortBindings = bindings
		},
		// A static address per node keeps --raft.initial-members made of plain
		// IPs, exactly as in a real deployment: the replica ID is the position
		// in that list, so the addresses must be known before the nodes start.
		EndpointSettingsModifier: func(settings map[string]*dnetwork.EndpointSettings) {
			for _, s := range settings {
				s.IPAMConfig = &dnetwork.EndpointIPAMConfig{IPv4Address: ip}
			}
		},
		// /healthz is a static 200, so this only proves the process got far
		// enough to serve admin traffic. Cluster readiness is asserted
		// separately, by a write.
		WaitingFor: wait.ForHTTP("/healthz").
			WithPort(fmt.Sprintf("%d/tcp", portREST)).
			WithStartupTimeout(2 * time.Minute),
	}
}

func (g *Group) mustStart(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := g.Start(ctx); err != nil {
		t.Fatalf("start %s cluster: %v", g.Role, err)
	}
	infof("%s cluster up (%d nodes)", g.Role, len(g.Nodes))
}

// Start creates any missing containers and starts the stopped ones.
func (g *Group) Start(ctx context.Context) error {
	for _, n := range g.Nodes {
		if err := n.start(ctx); err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
	}
	return nil
}

func (n *Node) start(ctx context.Context) error {
	if n.ctr != nil {
		return n.ctr.Start(ctx)
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: n.request(),
		Started:          true,
	})
	if err != nil {
		// Terminate whatever was created so a retry is not blocked by a
		// half-built container holding the name and the fixed ports.
		if ctr != nil {
			_ = ctr.Terminate(ctx)
		}
		return err
	}
	n.ctr = ctr
	return nil
}

// stopGrace is how long a node gets to shut down on SIGTERM before Docker
// escalates to SIGKILL.
//
// It is deliberately generous: several scenarios stop the follower cluster and
// restart it on the same data, and a clean stop is what makes that a restart
// rather than a second power-loss test. Stop reports how long each node
// actually took, because a node that routinely runs into this limit is a
// shutdown problem worth knowing about — and it silently doubles the runtime of
// the suite.
const stopGrace = 30 * time.Second

// Stop shuts every node down gracefully, keeping its container (and therefore
// its accumulated log) and its data directory.
func (g *Group) Stop(ctx context.Context) error {
	timeout := stopGrace
	for _, n := range g.Nodes {
		if n.ctr == nil {
			continue
		}
		n.closeConn()
		started := time.Now()
		if err := n.ctr.Stop(ctx, &timeout); err != nil {
			return fmt.Errorf("stop %s: %w", n.Name, err)
		}
		if took := time.Since(started); took > stopGrace-2*time.Second {
			infof("%s did not exit on SIGTERM within %s and was killed by docker", n.Name, stopGrace)
		} else {
			infof("%s stopped in %s", n.Name, took.Round(time.Millisecond))
		}
	}
	return nil
}

// Kill SIGKILLs one node: power loss, with no graceful shutdown and no flush.
func (g *Group) Kill(ctx context.Context, i int) error {
	n := g.Nodes[i]
	if n.ctr == nil {
		return fmt.Errorf("%s was never started", n.Name)
	}
	n.closeConn()
	_, err := g.env.docker.ContainerKill(ctx, n.ctr.GetContainerID(),
		dclient.ContainerKillOptions{Signal: "SIGKILL"})
	if err != nil {
		return fmt.Errorf("kill %s: %w", n.Name, err)
	}
	return nil
}

// Revive restarts a node after Kill or Stop. Its data directory is untouched,
// so it comes back with whatever state survived.
func (g *Group) Revive(ctx context.Context, i int) error {
	return g.Nodes[i].start(ctx)
}

// Rebuild destroys every container so that the next Start creates fresh ones
// from the current command line. Needed when a scenario changes a flag — the
// snapshot source, say — because argv is fixed at container creation.
//
// Data directories survive; container logs do not, so they are saved first.
// Rebuild deliberately does not start the new containers: a scenario has to be
// able to take a log mark before the node writes its first line.
func (g *Group) Rebuild(ctx context.Context) {
	for _, n := range g.Nodes {
		n.saveLogs(ctx)
		n.terminate(ctx)
	}
	g.refreshCmds()
}

// SetLeaderFullInterval changes how often the leader publishes a full snapshot
// and restarts the leader cluster so the new value takes effect.
//
// The interval ticker publishes fulls unconditionally
// (replication/store/exporter.go, Run), and a full lands in the store with a
// tip past a stopped follower's index — which shuts the delta window for that
// follower permanently, because every later base chains off that tip. The
// incremental scenario therefore has to silence the ticker for the duration of
// its setup. Everything else wants it short, so that a usable base exists
// seconds after startup.
//
// Restarting the leader is safe at any point: the data directories survive and
// the follower simply retries its replication stream.
func (e *Env) SetLeaderFullInterval(ctx context.Context, d time.Duration) {
	e.t.Helper()
	e.cfg.FullInterval = d
	e.Leaders.refreshCmds()
	e.Leaders.Rebuild(ctx)
	if err := e.Leaders.Start(ctx); err != nil {
		e.t.Fatalf("restart leader cluster with full-interval=%s: %v", d, err)
	}
	e.awaitLeaderTable(ctx)
	infof("leader cluster restarted with shared-store.full-interval=%s", d)
}

// SetSnapshotSource switches the follower cluster between proxy and direct mode.
// Takes effect on the next Rebuild.
func (g *Group) SetSnapshotSource(s SnapshotSource) {
	g.source = s
	g.refreshCmds()
}

// WipeData destroys every node's raft log and state machine, which forces a
// full recovery: no local shard, nothing to apply a delta onto. The nodes must
// be stopped.
func (g *Group) WipeData(t *testing.T) {
	t.Helper()
	for _, n := range g.Nodes {
		for _, sub := range []string{"raft", "state-machine"} {
			p := filepath.Join(n.dataDir, sub)
			if err := os.RemoveAll(p); err != nil {
				t.Fatalf("wipe %s: %v", p, err)
			}
			mkdirAllShared(t, p)
		}
	}
}

func (n *Node) terminate(ctx context.Context) {
	if n.ctr == nil {
		return
	}
	n.closeConn()
	_ = n.ctr.Terminate(ctx)
	n.ctr = nil
}

// ── logs ─────────────────────────────────────────────────────────────────────

// Logs returns everything the node has written since its container was created,
// including across restarts.
func (n *Node) Logs(ctx context.Context) string {
	if n.ctr == nil {
		return ""
	}
	rc, err := n.ctr.Logs(ctx)
	if err != nil {
		return ""
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil && !errors.Is(err, io.EOF) {
		return string(b)
	}
	return string(b)
}

func (n *Node) saveLogs(ctx context.Context) {
	if n.ctr == nil {
		return
	}
	out := n.Logs(ctx)
	if out == "" {
		return
	}
	path := filepath.Join(n.env.LogDir, n.Name+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(out)
}

// ── clients ──────────────────────────────────────────────────────────────────

// Conn returns a lazily dialled, cached gRPC connection to this node's API.
//
// The host port is fixed, so the connection survives a container restart:
// grpc-go reconnects on its own once the listener is back.
func (n *Node) Conn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		return n.conn
	}
	conn, err := grpc.NewClient(n.APIAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
	)
	if err != nil {
		t.Fatalf("dial %s (%s): %v", n.Name, n.APIAddr, err)
	}
	n.conn = conn
	return conn
}

func (n *Node) closeConn() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conn != nil {
		_ = n.conn.Close()
		n.conn = nil
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mkdirAllShared(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	// MkdirAll honours umask, so set the mode explicitly.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod %s: %v", dir, err)
	}
}

// tcpPort renders an internal container port for the Docker API.
func tcpPort(p int) dnetwork.Port {
	return dnetwork.MustParsePort(fmt.Sprintf("%d/tcp", p))
}

func portOf(hostPort string) string {
	_, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return p
}

// sanitize keeps container names to characters Docker accepts.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}
