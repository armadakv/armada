// Copyright JAMF Software, LLC

package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/armadakv/armada/replication/store"
	"github.com/armadakv/objfs"
	objfaszblob "github.com/armadakv/objfs/azblob"
	objfgcs "github.com/armadakv/objfs/gcs"
	objfss3 "github.com/armadakv/objfs/s3"
	"github.com/spf13/viper"
)

type BucketConfig struct {
	Backend string
	// Filesystem backend
	Directory string
	// S3 backend
	S3Bucket string
	// GCS backend
	GCSBucket string
	// Azure Blob Storage backend
	AzureContainer string
	AzureAccount   string
	AzureKey       string
}

func (cfg *BucketConfig) validate() error {
	switch cfg.Backend {
	case "", "none":
		return nil
	case "filesystem":
		if cfg.Directory == "" {
			return fmt.Errorf("filesystem config missing 'directory'")
		}
	case "s3":
		if cfg.S3Bucket == "" {
			return fmt.Errorf("s3 config missing 'bucket'")
		}
	case "gcs":
		if cfg.GCSBucket == "" {
			return fmt.Errorf("gcs config missing 'bucket'")
		}
	case "azblob":
		if cfg.AzureContainer == "" || cfg.AzureAccount == "" || cfg.AzureKey == "" {
			return fmt.Errorf("azure config missing 'container', 'account', or 'key'")
		}
	default:
		return fmt.Errorf("unsupported backend %q (supported: none, filesystem, s3, gcs, azblob)", cfg.Backend)
	}
	return nil
}

// newBucketFromConfig creates an objfs.Bucket from the given BucketConfig.
func newBucketFromConfig(ctx context.Context, cfg BucketConfig) (objfs.Bucket, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	switch cfg.Backend {
	case "", "none":
		return nil, nil
	case "filesystem":
		absDir, err := filepath.Abs(cfg.Directory)
		if err != nil {
			return nil, fmt.Errorf("invalid directory path: %w", err)
		}
		bkt, err := objfs.NewLocal(absDir)
		if err != nil {
			return nil, fmt.Errorf("create filesystem bucket: %w", err)
		}
		return bkt, nil
	case "s3":
		bkt, err := objfss3.Open(ctx, cfg.S3Bucket)
		if err != nil {
			return nil, fmt.Errorf("create s3 bucket: %w", err)
		}
		return bkt, nil
	case "gcs":
		bkt, err := objfgcs.Open(ctx, cfg.GCSBucket)
		if err != nil {
			return nil, fmt.Errorf("create gcs bucket: %w", err)
		}
		return bkt, nil
	case "azblob":
		bkt, err := objfaszblob.OpenWithSharedKey(cfg.AzureContainer, cfg.AzureAccount, cfg.AzureKey)
		if err != nil {
			return nil, fmt.Errorf("create azure blob bucket: %w", err)
		}
		return bkt, nil
	default:
		return nil, fmt.Errorf("unsupported backend %q (supported: none, filesystem, s3, gcs, azblob)", cfg.Backend)
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
