package integration

import (
	"net/netip"
	"os"
	"strconv"
	"testing"
	"time"
)

// Config collects every knob the harness exposes. All of them have defaults
// that make the full suite pass on a laptop; the environment variables exist
// for debugging and for tuning on slower CI machines.
type Config struct {
	// Table is the table the scenarios read and write.
	Table string
	// Root is the host directory for data, the shared store and saved logs.
	// Empty means use the test's temporary directory, which is removed
	// afterwards. Set ARMADA_TEST_ROOT to keep the tree for inspection.
	Root string
	// Keep leaves the containers running after the test, for manual poking.
	Keep bool

	Subnet      string
	LeaderIPs   []string
	FollowerIPs []string
	PortBase    int

	// Keys is the size of the baseline data set.
	Keys int
	// IncrGap bounds how far the follower may be pushed behind while the
	// incremental scenario tries to open the delta window.
	IncrGap int
	// IncrChunk is how many keys that scenario writes between checks of the
	// shared store. It has to be small: the window it is hunting for is only a
	// few hundred raft entries wide, so writing the whole gap in one go
	// overshoots it.
	IncrChunk int
	// Concurrency is the number of in-flight Put streams used by the loader.
	Concurrency int

	// LeaderSnapshotEntries and LeaderCompactionOverhead are the shipped
	// defaults (10000/5000, cmd/armada/flags.go) divided by ten, so that a few
	// thousand writes produce several compactions instead of none. What is
	// preserved is the 2:1 *ratio*, and that is the part that matters.
	//
	// The exporter refuses to publish a delta whose base is at or below the GC
	// horizon, and it used to chain the base onto the previous artefact's tip.
	// Exports are driven by log compaction, so that tip sits about
	// snapshot-entries behind the current index while the horizon is only about
	// compaction-overhead behind it — so a delta was publishable only when
	// snapshot-entries < compaction-overhead. Every real configuration has that
	// the other way round, which forced every export to a full and made the
	// incremental path unreachable rather than merely rare.
	//
	// ExportIncremental now re-bases at the horizon rather than giving up, so
	// deltas are publishable at any ratio. Keeping the production ratio here is
	// what keeps that honest: under the old behaviour these values reproduce
	// the failure, so if it ever returns the incremental scenario stops
	// engaging and reports the tips and horizon it measured.
	LeaderSnapshotEntries      int
	LeaderCompactionOverhead   int
	FollowerSnapshotEntries    int
	FollowerCompactionOverhead int
	// FullInterval is how often the leader publishes a full snapshot. It is
	// short so that a usable base exists seconds after startup, but the ticker
	// competes with the delta window — see Env.SetLeaderFullInterval.
	FullInterval time.Duration
	IncrMaxChain int

	// Timeout scales every wait in the suite. Bump it on a slow host instead of
	// editing individual scenarios.
	Timeout time.Duration
}

// LoadConfig reads the harness configuration from the environment.
func LoadConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{
		Table:                      envString("ARMADA_TEST_TABLE", "armada-test"),
		Root:                       os.Getenv("ARMADA_TEST_ROOT"),
		Keep:                       os.Getenv("ARMADA_TEST_KEEP") != "",
		Subnet:                     envString("ARMADA_TEST_SUBNET", "10.99.0.0/16"),
		PortBase:                   envInt(t, "ARMADA_TEST_PORT_BASE", 15000),
		Keys:                       envInt(t, "ARMADA_TEST_KEYS", 2000),
		IncrGap:                    envInt(t, "ARMADA_TEST_INCR_GAP", 4000),
		IncrChunk:                  envInt(t, "ARMADA_TEST_INCR_CHUNK", 150),
		Concurrency:                envInt(t, "ARMADA_TEST_CONCURRENCY", 32),
		LeaderSnapshotEntries:      envInt(t, "ARMADA_TEST_LEADER_SNAPSHOT_ENTRIES", 1000),
		LeaderCompactionOverhead:   envInt(t, "ARMADA_TEST_LEADER_COMPACTION_OVERHEAD", 500),
		FollowerSnapshotEntries:    envInt(t, "ARMADA_TEST_FOLLOWER_SNAPSHOT_ENTRIES", 200),
		FollowerCompactionOverhead: envInt(t, "ARMADA_TEST_FOLLOWER_COMPACTION_OVERHEAD", 20),
		FullInterval:               envDuration(t, "ARMADA_TEST_FULL_INTERVAL", 30*time.Second),
		IncrMaxChain:               envInt(t, "ARMADA_TEST_INCR_MAX_CHAIN", 3),
		Timeout:                    envDuration(t, "ARMADA_TEST_TIMEOUT_SCALE", 0),
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = time.Second // scale factor of 1
	}
	prefix, err := netip.ParsePrefix(cfg.Subnet)
	if err != nil {
		t.Fatalf("invalid ARMADA_TEST_SUBNET %q: %v", cfg.Subnet, err)
	}
	// Leaders at .11-.13, followers at .21-.23, leaving the low addresses to
	// the bridge gateway.
	for i := range 3 {
		cfg.LeaderIPs = append(cfg.LeaderIPs, offsetAddr(t, prefix.Addr(), 11+i))
		cfg.FollowerIPs = append(cfg.FollowerIPs, offsetAddr(t, prefix.Addr(), 21+i))
	}
	return cfg
}

// hostAPIPort and hostRESTPort lay the published ports out so that they read at
// a glance: 15001-15003 leader API, 15011-15013 leader admin, 15021-15023
// follower API, 15031-15033 follower admin.
func (c Config) hostAPIPort(role Role, i int) int {
	if role == RoleLeader {
		return c.PortBase + 1 + i
	}
	return c.PortBase + 21 + i
}

func (c Config) hostRESTPort(role Role, i int) int {
	if role == RoleLeader {
		return c.PortBase + 11 + i
	}
	return c.PortBase + 31 + i
}

func (c Config) allHostPorts() []int {
	var ports []int
	for _, role := range []Role{RoleLeader, RoleFollower} {
		for i := range 3 {
			ports = append(ports, c.hostAPIPort(role, i), c.hostRESTPort(role, i))
		}
	}
	return ports
}

// scale converts a nominal timeout into a wall-clock one.
func (c Config) scale(d time.Duration) time.Duration {
	return time.Duration(float64(d) * float64(c.Timeout) / float64(time.Second))
}

func offsetAddr(t *testing.T, base netip.Addr, n int) string {
	t.Helper()
	a := base
	for range n {
		a = a.Next()
		if !a.IsValid() {
			t.Fatalf("subnet too small to hold %d addresses", n)
		}
	}
	return a.String()
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return n
}

func envDuration(t *testing.T, key string, def time.Duration) time.Duration {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("invalid %s=%q: %v", key, v, err)
	}
	return d
}
