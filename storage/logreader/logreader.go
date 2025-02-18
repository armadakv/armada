// Copyright JAMF Software, LLC

package logreader

import (
	"context"

	"github.com/armadakv/armada/raft"
	"github.com/armadakv/armada/raft/logreader"
	"github.com/armadakv/armada/raft/raftpb"
	serrors "github.com/armadakv/armada/storage/errors"
)

type Interface interface {
	// QueryRaftLog for all the entries in a given cluster within the right half-open range
	// defined by dragonboat.LogRange. MaxSize denotes the maximum cumulative size of the entries,
	// but this serves only as a hint and the actual size of returned entries may be larger than maxSize.
	QueryRaftLog(context.Context, uint64, raft.LogRange, uint64) ([]raftpb.Entry, error)
}

type logQuerier interface {
	GetLogReader(shardID uint64) (*logreader.LogReader, error)
}

type Simple struct {
	LogQuerier logQuerier
}

func (l *Simple) QueryRaftLog(ctx context.Context, clusterID uint64, logRange raft.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	// Empty log range should return immediately.
	if logRange.FirstIndex == logRange.LastIndex {
		return nil, nil
	}
	return readLog(l.LogQuerier, clusterID, logRange, maxSize)
}

func readLog(q logQuerier, clusterID uint64, logRange raft.LogRange, maxSize uint64) ([]raftpb.Entry, error) {
	r, err := q.GetLogReader(clusterID)
	if err != nil {
		return nil, err
	}

	rFirst, rLast := r.GetRange()
	// Follower is up-to-date with the leader, therefore there are no new data to be sent.
	if rLast+1 == logRange.FirstIndex {
		return nil, nil
	}
	// Follower is ahead of the leader, has to be manually fixed.
	if rLast < logRange.FirstIndex {
		return nil, serrors.ErrLogBehind
	}
	// Follower's leaderIndex is in the leader's snapshot, not in the log.
	if logRange.FirstIndex < rFirst {
		return nil, serrors.ErrLogAhead
	}

	return r.Entries(logRange.FirstIndex, logRange.LastIndex, maxSize)
}
