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
	// revision is the table revision after the entries applied so far.
	revision        uint64
	revisionChanged bool
	// events records the changes of the batch, nil when nobody watches.
	events *eventRecorder
	// rewritten is set when the batch rewrites history instead of extending it.
	rewritten bool
	// floor is the history floor after the entries applied so far.
	floor        uint64
	floorChanged bool
}

// raiseFloor marks the revisions up to r as collapsed by a snapshot replay.
func (c *updateContext) raiseFloor(r uint64) {
	if r > c.floor {
		c.floor = r
		c.floorChanged = true
	}
}

// capturing reports whether the batch records events.
func (c *updateContext) capturing() bool {
	return c.events != nil
}

// recordEvent records one change, unless it belongs to a snapshot replay:
// replayed records carry their source revisions and rewrite history.
func (c *updateContext) recordEvent(typ armadapb.Event_EventType, userKey, value []byte, createSeqno, modSeqno uint64, prev *armadapb.KeyValue, replayed bool) {
	if replayed {
		c.rewritten = true
		c.raiseFloor(modSeqno)
		return
	}
	if c.events != nil {
		c.events.record(newEvent(typ, userKey, value, createSeqno, modSeqno, prev))
	}
}

// advanceRevision updates the table revision after an entry was applied.
func (c *updateContext) advanceRevision() {
	cmd := c.cmd
	switch {
	case cmd.Type == armadapb.Command_GC:
		// GC proposed on a cluster's own Raft leader carries the compacted
		// local index, which is not a revision.
		return
	case cmd.Type == armadapb.Command_DUMMY && cmd.LeaderIndex != nil && *cmd.LeaderIndex == 0:
		// A maintenance reset restarts replication from scratch.
		c.rewritten = true
		c.setRevision(0)
	case cmd.Type == armadapb.Command_DUMMY && cmd.LeaderIndex != nil:
		// A top-level DUMMY with a source index is the terminal marker of a
		// snapshot replay; live replication always wraps commands in a SEQUENCE.
		// The replay collapsed every revision up to the artifact tip, including
		// keys that were created and deleted again and so left no record.
		c.rewritten = true
		c.setRevision(*cmd.LeaderIndex)
		c.raiseFloor(*cmd.LeaderIndex)
	case cmd.Type == armadapb.Command_SEQUENCE:
		r := sequenceRevision(cmd)
		if r == 0 {
			// Children without source indexes are written at the entry's sequence.
			r = c.seqno()
		}
		c.setRevision(r)
	default:
		c.setRevision(c.seqno())
	}
}

func (c *updateContext) setRevision(r uint64) {
	c.revision = r
	c.revisionChanged = true
}

// commit returns the observer notification for the committed batch.
func (c *updateContext) commit() Commit {
	res := Commit{Revision: c.revision, Captured: c.events != nil, Rewritten: c.rewritten}
	if c.events != nil {
		res.Events = c.events.sorted()
	}
	return res
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
	if c.floorChanged {
		floor := make([]byte, 8)
		binary.LittleEndian.PutUint64(floor, c.floor)
		if err := c.batch.Set(sysHistoryFloor, floor, nil); err != nil {
			return err
		}
	}
	if c.revisionChanged {
		rev := make([]byte, 8)
		binary.LittleEndian.PutUint64(rev, c.revision)
		if err := c.batch.Set(sysRevision, rev, nil); err != nil {
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
