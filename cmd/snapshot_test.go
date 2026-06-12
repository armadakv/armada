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

func TestNewSharedStoreBucket_None(t *testing.T) {
	bkt, err := newSharedStoreBucket("none", "")
	require.NoError(t, err)
	assert.Nil(t, bkt, "none backend should return nil bucket")

	bkt, err = newSharedStoreBucket("", "")
	require.NoError(t, err)
	assert.Nil(t, bkt, "empty backend should return nil bucket")
}

func TestNewSharedStoreBucket_Unsupported(t *testing.T) {
	_, err := newSharedStoreBucket("s3", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported backend")
}

func TestNewSharedStoreBucket_Filesystem(t *testing.T) {
	dir := t.TempDir()
	cfg := "directory: " + dir
	bkt, err := newSharedStoreBucket("filesystem", cfg)
	require.NoError(t, err)
	require.NotNil(t, bkt)

	_, ok := bkt.(objstore.Bucket)
	assert.True(t, ok)
}

func TestNewSharedStoreBucket_FilesystemMissingDirectory(t *testing.T) {
	_, err := newSharedStoreBucket("filesystem", "")
	require.Error(t, err)
}

func TestReplicationExporterConfig_Defaults(t *testing.T) {
	initConfig(leaderCmd.PersistentFlags())

	cfg := replicationExporterConfig("node-1", nil)
	assert.Equal(t, "node-1", cfg.NodeID)
	assert.Equal(t, 10*time.Minute, cfg.SnapshotTimeout)
}

func TestSharedStoreGCConfig_Defaults(t *testing.T) {
	initConfig(leaderCmd.PersistentFlags())

	cfg := sharedStoreGCConfig(nil)
	assert.Equal(t, 48*time.Hour, cfg.Retention)
	assert.Equal(t, time.Hour, cfg.Interval)
}

func init() {
	if os.Getenv("SKIP_LEADER_INIT") == "" {
		os.Setenv("SKIP_LEADER_INIT", "1")
	}
}
