// Copyright JAMF Software, LLC

package storage

import (
	"testing"

	"github.com/armadakv/armada/raft/raftio"
	"github.com/stretchr/testify/require"
)

func TestEventsCoalesceClusterRefreshSignals(t *testing.T) {
	e := newEvents(nil)
	e.LeaderUpdated(raftio.LeaderInfo{})
	e.NodeReady(raftio.NodeInfo{})
	e.NodeUnloaded(raftio.NodeInfo{})
	e.MembershipChanged(raftio.NodeInfo{})
	e.NodeDeleted(raftio.NodeInfo{})

	require.Len(t, e.refreshCh, 1)
	e.Stop()
}

func TestEventsRetainHighestCompactionIndexPerShard(t *testing.T) {
	e := newEvents(nil)
	e.LogCompacted(raftio.EntryInfo{ShardID: 1, Index: 10})
	e.LogCompacted(raftio.EntryInfo{ShardID: 2, Index: 20})
	e.LogCompacted(raftio.EntryInfo{ShardID: 1, Index: 30})
	e.LogCompacted(raftio.EntryInfo{ShardID: 2, Index: 15})

	require.Len(t, e.compactionCh, 1)
	require.Equal(t, map[uint64]uint64{1: 30, 2: 20}, e.takeCompactions())
	e.Stop()
}

func TestEventsStopPreventsFurtherEnqueue(t *testing.T) {
	e := newEvents(nil)
	e.Stop()
	e.LeaderUpdated(raftio.LeaderInfo{})
	e.LogCompacted(raftio.EntryInfo{ShardID: 1, Index: 1})
	e.NodeReady(raftio.NodeInfo{})

	require.Empty(t, e.refreshCh)
	require.Empty(t, e.compactionCh)
	require.Empty(t, e.compactions)
}
