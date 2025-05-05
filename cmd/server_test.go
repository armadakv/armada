// Copyright 2024 - 2025 Jakub Coufal (coufalja@gmail.com) and other contributors.

package cmd

import (
	"os"
	"testing"
	"time"

	"github.com/armadakv/armada/storage/table"
	"github.com/spf13/viper"
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

func TestParseInitialMembers(t *testing.T) {
	tests := []struct {
		name    string
		members map[string]string
		want    map[uint64]string
		wantErr bool
	}{
		{
			name: "valid",
			members: map[string]string{
				"1": "localhost:8080",
				"2": "localhost:8081",
				"3": "localhost:8082",
			},
			want: map[uint64]string{
				1: "localhost:8080",
				2: "localhost:8081",
				3: "localhost:8082",
			},
			wantErr: false,
		},
		{
			name: "invalid",
			members: map[string]string{
				"1":    "localhost:8080",
				"2":    "localhost:8081",
				"test": "localhost:8082",
			},
			want:    nil,
			wantErr: true,
		},
		{
			name:    "empty",
			members: map[string]string{},
			want:    map[uint64]string{},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInitialMembers(tt.members)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSetupRESTServer(t *testing.T) {
	// Save the original value of the rest.address config
	originalAddress := viper.GetString("rest.address")
	originalTimeout := viper.GetDuration("rest.read-timeout")
	defer func() {
		// Restore the original value
		viper.Set("rest.address", originalAddress)
		viper.Set("rest.read-timeout", originalTimeout)
	}()

	// Set up a test address and timeout
	testAddr := "http://localhost:0" // Use port 0 to let the OS choose a free port
	testTimeout := 5 * time.Second
	viper.Set("rest.address", testAddr)
	viper.Set("rest.read-timeout", testTimeout)

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
	originalValues := map[string]interface{}{
		"api.advertise-address":        viper.Get("api.advertise-address"),
		"raft.node-id":                 viper.Get("raft.node-id"),
		"raft.initial-members":         viper.Get("raft.initial-members"),
		"raft.wal-dir":                 viper.Get("raft.wal-dir"),
		"raft.node-host-dir":           viper.Get("raft.node-host-dir"),
		"raft.rtt":                     viper.Get("raft.rtt"),
		"raft.address":                 viper.Get("raft.address"),
		"raft.listen-address":          viper.Get("raft.listen-address"),
		"raft.max-recv-queue-size":     viper.Get("raft.max-recv-queue-size"),
		"raft.max-send-queue-size":     viper.Get("raft.max-send-queue-size"),
		"memberlist.address":           viper.Get("memberlist.address"),
		"memberlist.advertise-address": viper.Get("memberlist.advertise-address"),
		"memberlist.members":           viper.Get("memberlist.members"),
		"memberlist.cluster-name":      viper.Get("memberlist.cluster-name"),
		"memberlist.node-name":         viper.Get("memberlist.node-name"),
		"raft.election-rtt":            viper.Get("raft.election-rtt"),
		"raft.heartbeat-rtt":           viper.Get("raft.heartbeat-rtt"),
		"raft.snapshot-entries":        viper.Get("raft.snapshot-entries"),
		"raft.compaction-overhead":     viper.Get("raft.compaction-overhead"),
		"raft.max-in-mem-log-size":     viper.Get("raft.max-in-mem-log-size"),
		"raft.state-machine-dir":       viper.Get("raft.state-machine-dir"),
		"raft.snapshot-recovery-type":  viper.Get("raft.snapshot-recovery-type"),
		"storage.block-cache-size":     viper.Get("storage.block-cache-size"),
		"storage.table-cache-size":     viper.Get("storage.table-cache-size"),
	}
	defer func() {
		// Restore the original values
		for k, v := range originalValues {
			viper.Set(k, v)
		}
	}()

	// Set up test values
	viper.Set("api.advertise-address", "http://localhost:8443")
	viper.Set("raft.node-id", uint64(1))
	viper.Set("raft.initial-members", map[string]string{"1": "localhost:8080"})
	viper.Set("raft.wal-dir", "/tmp/wal")
	viper.Set("raft.node-host-dir", "/tmp/node")
	viper.Set("raft.rtt", 50*time.Millisecond)
	viper.Set("raft.address", "localhost:8080")
	viper.Set("raft.listen-address", "")
	viper.Set("raft.max-recv-queue-size", uint64(0))
	viper.Set("raft.max-send-queue-size", uint64(0))
	viper.Set("memberlist.address", "localhost:7432")
	viper.Set("memberlist.advertise-address", "")
	viper.Set("memberlist.members", []string{""})
	viper.Set("memberlist.cluster-name", "test")
	viper.Set("memberlist.node-name", "")
	viper.Set("raft.election-rtt", uint64(20))
	viper.Set("raft.heartbeat-rtt", uint64(1))
	viper.Set("raft.snapshot-entries", uint64(10000))
	viper.Set("raft.compaction-overhead", uint64(5000))
	viper.Set("raft.max-in-mem-log-size", uint64(6*1024*1024))
	viper.Set("raft.state-machine-dir", "/tmp/state-machine")
	viper.Set("raft.snapshot-recovery-type", "checkpoint")
	viper.Set("storage.block-cache-size", int64(16*1024*1024))
	viper.Set("storage.table-cache-size", 1024)

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
	require.Equal(t, "localhost:7432", config.Gossip.BindAddress)
	require.Empty(t, config.Gossip.AdvertiseAddress)
	require.Equal(t, []string{""}, config.Gossip.InitialMembers)
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
