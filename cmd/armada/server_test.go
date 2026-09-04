// Copyright JAMF Software, LLC

package main

import (
	"os"
	"testing"
	"time"

	"github.com/armadakv/armada/storage/table"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestToRecoveryType(t *testing.T) {
	tests := []struct {
		name string
		str  string
		want table.SnapshotRecoveryType
	}{
		{
			name: "snapshot",
			str:  "snapshot",
			want: table.RecoveryTypeSnapshot,
		},
		{
			name: "checkpoint",
			str:  "checkpoint",
			want: table.RecoveryTypeCheckpoint,
		},
		{
			name: "empty",
			str:  "",
			want: table.RecoveryTypeCheckpoint, // Default for non-Windows
		},
		{
			name: "unknown",
			str:  "unknown",
			want: table.RecoveryTypeCheckpoint, // Default for non-Windows
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toRecoveryType(tt.str)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		name        string
		urlStr      string
		wantAddr    string
		wantSecure  bool
		wantNetwork string
	}{
		{
			name:        "http",
			urlStr:      "http://localhost:8080",
			wantAddr:    "localhost:8080",
			wantSecure:  false,
			wantNetwork: "tcp",
		},
		{
			name:        "https",
			urlStr:      "https://localhost:8443",
			wantAddr:    "localhost:8443",
			wantSecure:  true,
			wantNetwork: "tcp",
		},
		{
			name:        "unix",
			urlStr:      "unix:///tmp/socket",
			wantAddr:    "/tmp/socket",
			wantSecure:  false,
			wantNetwork: "unix",
		},
		{
			name:        "unixs",
			urlStr:      "unixs:///tmp/socket",
			wantAddr:    "/tmp/socket",
			wantSecure:  true,
			wantNetwork: "unix",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, secure, network := resolveURL(tt.urlStr)
			require.Equal(t, tt.wantAddr, addr)
			require.Equal(t, tt.wantSecure, secure)
			require.Equal(t, tt.wantNetwork, network)
		})
	}
}

func TestParseInitialMembersList(t *testing.T) {
	tests := []struct {
		name        string
		members     []string
		raftAddress string
		wantNodeID  uint64
		wantMembers map[uint64]string
		wantErr     bool
	}{
		{
			name:        "single node",
			members:     []string{"localhost:8080"},
			raftAddress: "localhost:8080",
			wantNodeID:  1,
			wantMembers: map[uint64]string{1: "localhost:8080"},
		},
		{
			name:        "three nodes, first",
			members:     []string{"localhost:8080", "localhost:8081", "localhost:8082"},
			raftAddress: "localhost:8080",
			wantNodeID:  1,
			wantMembers: map[uint64]string{1: "localhost:8080", 2: "localhost:8081", 3: "localhost:8082"},
		},
		{
			name:        "three nodes, second",
			members:     []string{"localhost:8080", "localhost:8081", "localhost:8082"},
			raftAddress: "localhost:8081",
			wantNodeID:  2,
			wantMembers: map[uint64]string{1: "localhost:8080", 2: "localhost:8081", 3: "localhost:8082"},
		},
		{
			name:        "three nodes, third",
			members:     []string{"localhost:8080", "localhost:8081", "localhost:8082"},
			raftAddress: "localhost:8082",
			wantNodeID:  3,
			wantMembers: map[uint64]string{1: "localhost:8080", 2: "localhost:8081", 3: "localhost:8082"},
		},
		{
			name:        "address not in list",
			members:     []string{"localhost:8080", "localhost:8081"},
			raftAddress: "localhost:9999",
			wantErr:     true,
		},
		{
			name:        "empty list",
			members:     []string{},
			raftAddress: "localhost:8080",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNodeID, gotMembers, err := parseInitialMembersList(tt.members, tt.raftAddress)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantNodeID, gotNodeID)
			require.Equal(t, tt.wantMembers, gotMembers)
		})
	}
}

func TestSetupRESTServer(t *testing.T) {
	// Save the original value of the rest.address config
	originalAddress := k.String("rest.address")
	originalTimeout := k.Duration("rest.read-timeout")
	defer func() {
		// Restore the original value
		k.Set("rest.address", originalAddress)
		k.Set("rest.read-timeout", originalTimeout)
	}()

	// Set up a test address and timeout
	testAddr := "http://localhost:0" // Use port 0 to let the OS choose a free port
	testTimeout := 5 * time.Second
	k.Set("rest.address", testAddr)
	k.Set("rest.read-timeout", testTimeout)

	// Create a test logger
	logger := zaptest.NewLogger(t).Sugar()

	// Set up the REST server
	server := setupRESTServer(logger)
	defer server.Shutdown()

	// Since the server is running on a random port, we can't make a real HTTP request to it.
	// Instead, we'll just check that the server was created successfully.
	require.NotNil(t, server)
}

func TestWaitForShutdown(t *testing.T) {
	// Create a test logger
	logger := zaptest.NewLogger(t).Sugar()

	// Create a shutdown channel
	shutdown := make(chan os.Signal, 1)

	// Start a goroutine to send a signal to the shutdown channel
	go func() {
		time.Sleep(100 * time.Millisecond)
		shutdown <- os.Interrupt
	}()

	// Wait for shutdown
	waitForShutdown(shutdown, logger)
}

func TestCreateEngineConfig(t *testing.T) {
	// Save the original values of the config
	originalValues := map[string]any{
		"api.advertise-address":        k.Get("api.advertise-address"),
		"raft.initial-members":         k.Get("raft.initial-members"),
		"raft.wal-dir":                 k.Get("raft.wal-dir"),
		"raft.node-host-dir":           k.Get("raft.node-host-dir"),
		"raft.rtt":                     k.Get("raft.rtt"),
		"raft.address":                 k.Get("raft.address"),
		"raft.listen-address":          k.Get("raft.listen-address"),
		"raft.max-recv-queue-size":     k.Get("raft.max-recv-queue-size"),
		"raft.max-send-queue-size":     k.Get("raft.max-send-queue-size"),
		"memberlist.address":           k.Get("memberlist.address"),
		"memberlist.advertise-address": k.Get("memberlist.advertise-address"),
		"memberlist.members":           k.Get("memberlist.members"),
		"memberlist.cluster-name":      k.Get("memberlist.cluster-name"),
		"memberlist.node-name":         k.Get("memberlist.node-name"),
		"raft.election-rtt":            k.Get("raft.election-rtt"),
		"raft.heartbeat-rtt":           k.Get("raft.heartbeat-rtt"),
		"raft.snapshot-entries":        k.Get("raft.snapshot-entries"),
		"raft.compaction-overhead":     k.Get("raft.compaction-overhead"),
		"raft.max-in-mem-log-size":     k.Get("raft.max-in-mem-log-size"),
		"raft.state-machine-dir":       k.Get("raft.state-machine-dir"),
		"raft.snapshot-recovery-type":  k.Get("raft.snapshot-recovery-type"),
		"storage.block-cache-size":     k.Get("storage.block-cache-size"),
		"storage.table-cache-size":     k.Get("storage.table-cache-size"),
	}
	defer func() {
		// Restore the original values
		for key, v := range originalValues {
			k.Set(key, v)
		}
	}()

	// Set up test values
	k.Set("api.advertise-address", "http://localhost:8443")
	k.Set("raft.initial-members", []string{"localhost:8080"})
	k.Set("raft.wal-dir", "/tmp/wal")
	k.Set("raft.node-host-dir", "/tmp/node")
	k.Set("raft.rtt", 50*time.Millisecond)
	k.Set("raft.address", "localhost:8080")
	k.Set("raft.listen-address", "")
	k.Set("raft.max-recv-queue-size", uint64(0))
	k.Set("raft.max-send-queue-size", uint64(0))
	k.Set("memberlist.advertise-address", "")
	k.Set("memberlist.members", []string{""})
	k.Set("memberlist.cluster-name", "test")
	k.Set("memberlist.node-name", "")
	k.Set("raft.election-rtt", uint64(20))
	k.Set("raft.heartbeat-rtt", uint64(1))
	k.Set("raft.snapshot-entries", uint64(10000))
	k.Set("raft.compaction-overhead", uint64(5000))
	k.Set("raft.max-in-mem-log-size", uint64(6*1024*1024))
	k.Set("raft.state-machine-dir", "/tmp/state-machine")
	k.Set("raft.snapshot-recovery-type", "checkpoint")
	k.Set("storage.block-cache-size", int64(16*1024*1024))
	k.Set("storage.table-cache-size", 1024)

	// Create a test logger
	logger := zaptest.NewLogger(t)

	// Create a test applied index listener
	var appliedTable string
	var appliedRev uint64
	appliedIndexListener := func(table string, rev uint64) {
		appliedTable = table
		appliedRev = rev
	}

	// Create the engine config
	config, err := createEngineConfig(logger, appliedIndexListener)
	require.NoError(t, err)

	// Check that the config has the expected values
	require.Equal(t, "http://localhost:8443", config.ClientAddress)
	require.Equal(t, uint64(1), config.NodeID)
	require.Equal(t, map[uint64]string{1: "localhost:8080"}, config.InitialMembers)
	require.Equal(t, "/tmp/wal", config.WALDir)
	require.Equal(t, "/tmp/node", config.NodeHostDir)
	require.Equal(t, uint64(50), config.RTTMillisecond)
	require.Equal(t, "localhost:8080", config.RaftAddress)
	require.Empty(t, config.ListenAddress)
	require.True(t, config.EnableMetrics)
	require.Equal(t, uint64(0), config.MaxReceiveQueueSize)
	require.Equal(t, uint64(0), config.MaxSendQueueSize)
	require.Empty(t, config.Gossip.AdvertiseAddress)
	require.Equal(t, []string{"localhost:8080"}, config.Gossip.InitialMembers)
	require.Equal(t, "test", config.Gossip.ClusterName)
	require.Empty(t, config.Gossip.NodeName)
	require.Equal(t, uint64(20), config.Table.ElectionRTT)
	require.Equal(t, uint64(1), config.Table.HeartbeatRTT)
	require.Equal(t, uint64(10000), config.Table.SnapshotEntries)
	require.Equal(t, uint64(5000), config.Table.CompactionOverhead)
	require.Equal(t, uint64(6*1024*1024), config.Table.MaxInMemLogSize)
	require.Equal(t, "/tmp/state-machine", config.Table.DataDir)
	require.Equal(t, table.RecoveryTypeCheckpoint, config.Table.RecoveryType)
	require.Equal(t, int64(16*1024*1024), config.Table.BlockCacheSize)
	require.Equal(t, 1024, config.Table.TableCacheSize)
	require.NotNil(t, config.Table.AppliedIndexListener)

	// Test the applied index listener
	config.Table.AppliedIndexListener("test-table", 123)
	require.Equal(t, "test-table", appliedTable)
	require.Equal(t, uint64(123), appliedRev)
}

// Note: We're not testing the waitForEngine function directly because it requires a *storage.Engine,
// which would be difficult to mock. Instead, we're testing the other functions in the server.go file.
