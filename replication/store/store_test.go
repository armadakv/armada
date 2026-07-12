// Copyright Armada Contributors

package store

import (
	"context"
	"testing"

	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/require"
)

// NewLocalBucket creates a new local filesystem bucket in a temporary directory.
func NewLocalBucket(t *testing.T) objfs.Bucket {
	t.Helper()
	b, err := objfs.NewLocal(t.TempDir())
	require.NoError(t, err)
	return b
}

// ObjectCount returns the number of objects in the bucket matching the prefix.
func ObjectCount(t *testing.T, b objfs.Bucket, prefix string) int {
	t.Helper()
	var count int
	err := b.List(context.Background(), prefix, func(a objfs.Attributes) error {
		count++
		return nil
	})
	require.NoError(t, err)
	return count
}
