// Copyright JAMF Software, LLC

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBucketFromConfig_None(t *testing.T) {
	bkt, err := newBucketFromConfig(context.Background(), BucketConfig{Backend: "none"})
	require.NoError(t, err)
	assert.Nil(t, bkt, "none backend should return nil bucket")

	bkt, err = newBucketFromConfig(context.Background(), BucketConfig{Backend: ""})
	require.NoError(t, err)
	assert.Nil(t, bkt, "empty backend should return nil bucket")
}

func TestNewBucketFromConfig_Unsupported(t *testing.T) {
	_, err := newBucketFromConfig(context.Background(), BucketConfig{Backend: "invalid-backend"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported backend")
}

func TestNewBucketFromConfig_S3MissingBucket(t *testing.T) {
	_, err := newBucketFromConfig(context.Background(), BucketConfig{Backend: "s3"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "s3 config missing 'bucket'")
}

func TestNewBucketFromConfig_Filesystem(t *testing.T) {
	dir := t.TempDir()
	bkt, err := newBucketFromConfig(context.Background(), BucketConfig{
		Backend:   "filesystem",
		Directory: dir,
	})
	require.NoError(t, err)
	require.NotNil(t, bkt)
}

func TestNewBucketFromConfig_FilesystemMissingDirectory(t *testing.T) {
	_, err := newBucketFromConfig(context.Background(), BucketConfig{Backend: "filesystem"})
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
