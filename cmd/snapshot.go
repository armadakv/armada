// Copyright JAMF Software, LLC

package cmd

import (
	"fmt"

	"github.com/armadakv/armada/storage/snapshot"
	"github.com/spf13/viper"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/providers/filesystem"
)

// newSnapshotBucket creates an objstore.Bucket from the snapshot-store configuration.
// Returns (nil, nil) when backend is "none" (feature disabled).
func newSnapshotBucket(backend, cfgYAML string) (objstore.Bucket, error) {
	switch backend {
	case "", "none":
		return nil, nil
	case "filesystem":
		bkt, err := filesystem.NewBucketFromConfig([]byte(cfgYAML))
		if err != nil {
			return nil, fmt.Errorf("snapshot-store: create filesystem bucket: %w", err)
		}
		return bkt, nil
	default:
		return nil, fmt.Errorf("snapshot-store: unsupported backend %q (supported: none or empty string to disable, filesystem)", backend)
	}
}

// snapshotExporterConfig reads the snapshot-store Viper configuration and returns
// an ExporterConfig. bucket must not be nil (caller is responsible for the check).
func snapshotExporterConfig(nodeID string, bucket objstore.Bucket) snapshot.ExporterConfig {
	return snapshot.ExporterConfig{
		Bucket:       bucket,
		NodeID:       nodeID,
		FullInterval: viper.GetDuration("replication.snapshot-store.full-interval"),
		IncrInterval: viper.GetDuration("replication.snapshot-store.incr-interval"),
		IncrMaxChain: viper.GetInt("replication.snapshot-store.incr-max-chain"),
	}
}

// snapshotGCConfig reads the snapshot-store GC Viper configuration.
func snapshotGCConfig(bucket objstore.Bucket) snapshot.GCConfig {
	return snapshot.GCConfig{
		Bucket:    bucket,
		Retention: viper.GetDuration("replication.snapshot-store.retention"),
		Interval:  viper.GetDuration("replication.snapshot-store.gc-interval"),
	}
}
