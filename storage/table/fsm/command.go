// Copyright JAMF Software, LLC

// Package fsm implements the Raft finite state machine for Armada tables.
// It applies committed Raft log entries as MVCC writes to a Pebble-backed key-value store,
// tracking both the local Raft apply index and, on follower clusters, the index of the last
// entry replicated from the leader cluster, which stamps the same revision on every cluster.
package fsm

import (
	"encoding/binary"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/cockroachdb/pebble/v2"
	pb "google.golang.org/protobuf/proto"
)

type updateContext struct {
	batch *pebble.Batch
	db    *pebble.DB
	// index is the local Raft index of the entry being applied.
	index uint64
	// leaderIndex is the leader_index field of the command being applied; a
	// SEQUENCE switches it to each child's while the child is applied. For a
	// command replicated or restored from the leader cluster it is the index of
	// the source entry in the leader cluster's log. Other commands leave it
	// unset, except a maintenance reset, which sets it to zero, and GC, which
	// carries the compacted index of the proposing cluster's own log.
	leaderIndex *uint64
	// cmd is the top-level command of the entry being applied.
	cmd *armadapb.Command

	// appliedLeaderIndex is persisted as sysLeaderIndex, from which follower
	// replication resumes, and reported to the applied-index callback. It is
	// the leaderIndex of the last applied entry that is not a top-level GC:
	// the compacted index a GC carries is not a leader index, and on a
	// follower cluster it is in the follower's own index domain.
	appliedLeaderIndex *uint64
	// nonGCApplied is set once an entry other than a top-level GC was applied.
	nonGCApplied bool
	// gcHorizon is the GC horizon after the entries applied so far. It never
	// decreases: versions below an applied horizon may already be swept.
	gcHorizon        uint64
	gcHorizonChanged bool
}

// advanceGCHorizon applies a GC horizon, ignoring one that would move it back.
func (c *updateContext) advanceGCHorizon(horizon uint64) {
	if horizon > c.gcHorizon {
		c.gcHorizon = horizon
		c.gcHorizonChanged = true
	}
}

// seqno returns the sequence number to use when encoding MVCC keys. A
// command replicated from the leader cluster uses its leaderIndex, so that
// every cluster stamps the same seqno into the same logical write. A command
// proposed locally has none and uses the local Raft index, which on the leader
// cluster is that same source index. A GC command's leaderIndex is the
// compacted index instead; GC writes no versions.
func (c *updateContext) seqno() uint64 {
	if c.leaderIndex != nil {
		return *c.leaderIndex
	}
	return c.index
}

func (c *updateContext) EnsureIndexed() error {
	if c.batch.Indexed() {
		return nil
	}

	indexed := c.db.NewIndexedBatch()
	if err := indexed.Apply(c.batch, nil); err != nil {
		return err
	}
	if err := c.batch.Close(); err != nil {
		return err
	}
	c.batch = indexed
	return nil
}

func (c *updateContext) Commit() error {
	// Persist the leader index replication resumes from, when the batch
	// applied a command that carries one.
	if c.appliedLeaderIndex != nil {
		leaderIdx := make([]byte, 8)
		binary.LittleEndian.PutUint64(leaderIdx, *c.appliedLeaderIndex)
		if err := c.batch.Set(sysLeaderIndex, leaderIdx, nil); err != nil {
			return err
		}
	}
	if c.gcHorizonChanged {
		horizon := make([]byte, 8)
		binary.LittleEndian.PutUint64(horizon, c.gcHorizon)
		if err := c.batch.Set(sysGCHorizon, horizon, nil); err != nil {
			return err
		}
	}
	// Set local index
	idx := make([]byte, 8)
	binary.LittleEndian.PutUint64(idx, c.index)
	if err := c.batch.Set(sysLocalIndex, idx, nil); err != nil {
		return err
	}
	return c.batch.Commit(pebble.NoSync)
}

func (c *updateContext) Close() error {
	if err := c.batch.Close(); err != nil {
		return err
	}
	return nil
}

func parseCommand(c *updateContext, entry sm.Entry) (command, error) {
	c.index = entry.Index
	cmd := &armadapb.Command{}
	if err := cmd.UnmarshalVTUnsafe(entry.Cmd); err != nil {
		return commandDummy{}, err
	}
	c.leaderIndex = cmd.LeaderIndex
	c.cmd = cmd
	if cmd.Type != armadapb.Command_GC {
		c.appliedLeaderIndex = cmd.LeaderIndex
		c.nonGCApplied = true
	}
	return wrapCommand(cmd), nil
}

func wrapCommand(cmd *armadapb.Command) command {
	switch cmd.Type {
	case armadapb.Command_PUT:
		return commandPut{cmd}
	case armadapb.Command_DELETE:
		return commandDelete{cmd}
	case armadapb.Command_PUT_BATCH:
		return commandPutBatch{cmd}
	case armadapb.Command_DELETE_BATCH:
		return commandDeleteBatch{cmd}
	case armadapb.Command_TXN:
		return commandTxn{cmd}
	case armadapb.Command_SEQUENCE:
		return commandSequence{cmd}
	case armadapb.Command_DUMMY:
		return commandDummy{}
	case armadapb.Command_GC:
		return commandGC{cmd}
	}
	panic("unknown command type")
}

type command interface {
	handle(*updateContext) (UpdateResult, *armadapb.CommandResult, error)
}

func wrapRequestOp(req pb.Message) *armadapb.RequestOp {
	switch op := req.(type) {
	case *armadapb.RequestOp_Range:
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestRange{RequestRange: op}}
	case *armadapb.RequestOp_Put:
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestPut{RequestPut: op}}
	case *armadapb.RequestOp_DeleteRange:
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: op}}
	}
	return nil
}

func wrapResponseOp(req pb.Message) *armadapb.ResponseOp {
	switch op := req.(type) {
	case *armadapb.ResponseOp_Range:
		return &armadapb.ResponseOp{Response: &armadapb.ResponseOp_ResponseRange{ResponseRange: op}}
	case *armadapb.ResponseOp_Put:
		return &armadapb.ResponseOp{Response: &armadapb.ResponseOp_ResponsePut{ResponsePut: op}}
	case *armadapb.ResponseOp_DeleteRange:
		return &armadapb.ResponseOp{Response: &armadapb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: op}}
	}
	return nil
}
