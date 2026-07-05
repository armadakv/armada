// Copyright JAMF Software, LLC

package cmd

import (
	"testing"
	"time"

	"github.com/armadakv/objfs"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSharedStoreBucket_None(t *testing.T) {
	bkt, err := newSharedStoreBucket("none")
	require.NoError(t, err)
	assert.Nil(t, bkt, "none backend should return nil bucket")

	bkt, err = newSharedStoreBucket("")
	require.NoError(t, err)
	assert.Nil(t, bkt, "empty backend should return nil bucket")
}

func TestNewSharedStoreBucket_Unsupported(t *testing.T) {
	_, err := newSharedStoreBucket("gcs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported backend")
}

func TestNewSharedStoreBucket_S3MissingBucket(t *testing.T) {
	viper.Set("shared-store.s3.bucket", "")
	_, err := newSharedStoreBucket("s3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared-store: s3 config missing 'bucket'")
}

func TestNewSharedStoreBucket_Filesystem(t *testing.T) {
	dir := t.TempDir()
	viper.Set("shared-store.filesystem.directory", dir)
	defer viper.Set("shared-store.filesystem.directory", "")
	bkt, err := newSharedStoreBucket("filesystem")
	require.NoError(t, err)
	require.NotNil(t, bkt)

	_, ok := bkt.(objfs.Bucket)
	assert.True(t, ok)
}

func TestNewSharedStoreBucket_FilesystemMissingDirectory(t *testing.T) {
	viper.Set("shared-store.filesystem.directory", "")
	_, err := newSharedStoreBucket("filesystem")
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
	assert.Equal(t, 1*time.Hour, cfg.Interval)
}
