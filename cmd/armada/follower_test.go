// Copyright Armada Contributors

package main

import (
	"path/filepath"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

func TestFollowerHTTPSnapshotSetup_HTTPDoesNotLoadTLSFiles(t *testing.T) {
	originalConfig := k
	k = koanf.New(".")
	t.Cleanup(func() { k = originalConfig })

	missingTLSFile := filepath.Join(t.TempDir(), "does-not-exist")
	k.Set("replication.leader-address", "http://leader.example:8444")
	k.Set("replication.cert-filename", missingTLSFile+".crt")
	k.Set("replication.key-filename", missingTLSFile+".key")
	k.Set("replication.ca-filename", missingTLSFile+".ca")
	k.Set("replication.insecure-skip-verify", true)
	k.Set("replication.server-name", "leader.example")
	k.Set("replication.snapshot-source", "proxy")
	k.Set("shared-store.backend", "none")
	k.Set("raft.address", "follower.example:8444")

	logger := zaptest.NewLogger(t)

	getter, err := createLeaderSnapshotHTTPGetter(logger)
	require.NoError(t, err)
	require.NotNil(t, getter)

	access, err := createSnapshotAccess(logger)
	require.NoError(t, err)
	require.NotNil(t, access.Objects)
	require.NotNil(t, access.Live)
	require.NotNil(t, access.Leases)
}
