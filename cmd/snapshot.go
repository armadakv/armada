// Copyright JAMF Software, LLC

package cmd

import (
	"fmt"

	"github.com/armadakv/armada/replication/store"
	"github.com/spf13/viper"
	"github.com/thanos-io/objstore"
	"github.com/thanos-io/objstore/providers/filesystem"
)

// newSharedStoreBucket creates an objstore.Bucket from the shared-store configuration.
// Returns (nil, nil) when backend is "none" (feature disabled).
func newSharedStoreBucket(backend, cfgYAML string) (objstore.Bucket, error) {
	switch backend {
	case "", "none":
		return nil, nil
	case "filesystem":
		bkt, err := filesystem.NewBucketFromConfig([]byte(cfgYAML))
		if err != nil {
			return nil, fmt.Errorf("shared-store: create filesystem bucket: %w", err)
		}
		return bkt, nil
	default:
		return nil, fmt.Errorf("shared-store: unsupported backend %q (supported: none or empty string to disable, filesystem)", backend)
	}
}

// replicationExporterConfig builds an ExporterConfig for the snapshot exporter
// from the replication-specific Viper keys.
func replicationExporterConfig(nodeID string, bucket objstore.Bucket) store.ExporterConfig {
	return store.ExporterConfig{
		Bucket:          bucket,
		NodeID:          nodeID,
		SnapshotTimeout: viper.GetDuration("replication.snapshot-timeout"),
	}
}

// sharedStoreGCConfig builds a GCConfig from the shared-store Viper keys.
func sharedStoreGCConfig(bucket objstore.Bucket) store.GCConfig {
	return store.GCConfig{
		Bucket:    bucket,
		Retention: viper.GetDuration("shared-store.retention"),
		Interval:  viper.GetDuration("shared-store.gc-interval"),
	}
}
