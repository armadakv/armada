// Copyright JAMF Software, LLC

package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
	objfss3 "github.com/armadakv/objfs/s3"
	"github.com/spf13/viper"
)

// newSharedStoreBucket creates an objfs.Bucket from the shared-store configuration.
// Returns (nil, nil) when backend is "none" (feature disabled).
func newSharedStoreBucket(backend string) (objfs.Bucket, error) {
	switch backend {
	case "", "none":
		return nil, nil
	case "filesystem":
		dir := viper.GetString("shared-store.filesystem.directory")
		if dir == "" {
			return nil, fmt.Errorf("shared-store: filesystem config missing 'directory' (set --shared-store.filesystem.directory)")
		}

		// Ensure absolute path, or relative to current working dir
		absDir, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("shared-store: invalid directory path: %w", err)
		}

		bkt, err := objfs.NewLocal(absDir)
		if err != nil {
			return nil, fmt.Errorf("shared-store: create filesystem bucket: %w", err)
		}
		return bkt, nil
	case "s3":
		bucket := viper.GetString("shared-store.s3.bucket")
		if bucket == "" {
			return nil, fmt.Errorf("shared-store: s3 config missing 'bucket' (set --shared-store.s3.bucket)")
		}
		bkt, err := objfss3.Open(context.Background(), bucket)
		if err != nil {
			return nil, fmt.Errorf("shared-store: create s3 bucket: %w", err)
		}
		return bkt, nil
	default:
		return nil, fmt.Errorf("shared-store: unsupported backend %q (supported: none, filesystem, s3)", backend)
	}
}

// replicationExporterConfig builds an ExporterConfig for the snapshot exporter
// from the replication-specific Viper keys.
func replicationExporterConfig(nodeID string, bucket objfs.Bucket) store.ExporterConfig {
	return store.ExporterConfig{
		Bucket:          bucket,
		NodeID:          nodeID,
		SnapshotTimeout: viper.GetDuration("replication.snapshot-timeout"),
	}
}

// sharedStoreGCConfig builds a GCConfig from the shared-store Viper keys.
func sharedStoreGCConfig(bucket objfs.Bucket) store.GCConfig {
	return store.GCConfig{
		Bucket:    bucket,
		Retention: viper.GetDuration("shared-store.retention"),
		Interval:  viper.GetDuration("shared-store.gc-interval"),
	}
}
