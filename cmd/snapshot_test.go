// Copyright JAMF Software, LLC

package cmd

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
)

func TestNewSnapshotBucket_None(t *testing.T) {
	bkt, err := newSnapshotBucket("none", "")
	require.NoError(t, err)
	assert.Nil(t, bkt, "none backend should return nil bucket")

	bkt, err = newSnapshotBucket("", "")
	require.NoError(t, err)
	assert.Nil(t, bkt, "empty backend should return nil bucket")
}

func TestNewSnapshotBucket_Unsupported(t *testing.T) {
	_, err := newSnapshotBucket("s3", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported backend")
}

func TestNewSnapshotBucket_Filesystem(t *testing.T) {
	dir := t.TempDir()
	cfg := "directory: " + dir
	bkt, err := newSnapshotBucket("filesystem", cfg)
	require.NoError(t, err)
	require.NotNil(t, bkt)

	// Verify it's a usable bucket.
	_, ok := bkt.(objstore.Bucket)
	assert.True(t, ok)
}

func TestNewSnapshotBucket_FilesystemMissingDirectory(t *testing.T) {
	_, err := newSnapshotBucket("filesystem", "")
	require.Error(t, err)
}

func TestSnapshotExporterConfig_Defaults(t *testing.T) {
	// Reset viper state, populate defaults (using flag defaults).
	initConfig(leaderCmd.PersistentFlags())

	cfg := snapshotExporterConfig("node-1", nil)
	assert.Equal(t, "node-1", cfg.NodeID)
	assert.Equal(t, 6*time.Hour, cfg.FullInterval)
	assert.Equal(t, 30*time.Minute, cfg.IncrInterval)
	assert.Equal(t, 8, cfg.IncrMaxChain)
}

func TestSnapshotGCConfig_Defaults(t *testing.T) {
	initConfig(leaderCmd.PersistentFlags())

	cfg := snapshotGCConfig(nil)
	assert.Equal(t, 48*time.Hour, cfg.Retention)
	assert.Equal(t, time.Hour, cfg.Interval)
}

func init() {
	// Ensure the leader command flags are registered before the tests run.
	// This is normally done by the init() in leader.go but may not happen
	// in test binaries so we guard with an env var to avoid double-registration.
	if os.Getenv("SKIP_LEADER_INIT") == "" {
		os.Setenv("SKIP_LEADER_INIT", "1")
	}
}
