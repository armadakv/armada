// Copyright Armada Contributors

// Package watch implements the server side of the Watch API.
//
// Every table shard served by a node has a Hub. The shard's state machine
// reports each committed apply batch to its hub, together with the events the
// batch produced, and the hub delivers those events to the watchers registered
// on it. Watchers write into a Sink, which is owned by one Watch stream and
// keeps the responses of all of its watchers in order.
//
// The apply path never waits for a watcher: the state machine hands batches to
// the hub without blocking, and the hub never waits for a stream.
package watch

import (
	"errors"
)

var (
	// ErrNotServed is returned when the table's serving shard does not run on this node.
	ErrNotServed = errors.New("table is not served by this node")
	// ErrHistoricalRevision is returned for a start revision that is not after the current revision.
	ErrHistoricalRevision = errors.New("watching from a past revision is not supported")
	// ErrClosed is returned once the registry is closed.
	ErrClosed = errors.New("watch registry is closed")
)

// Cancel reasons reported to clients.
const (
	reasonRewritten = "table history was rewritten by a recovery or a reset, re-list and watch again"
	reasonBehind    = "watcher fell behind the table, resume from the last received revision"
	reasonClosed    = "table is no longer served by this node"
	reasonDeleted   = "table was deleted"
	reasonReplaced  = "table was recreated or replaced by a recovery, re-list and watch again"
	reasonShutdown  = "node is shutting down"
)

// Options describe a watcher.
type Options struct {
	// ID identifies the watcher on its stream; it is echoed in every Message.
	ID int64
	// Table is the name of the watched table.
	Table string
	// Key and RangeEnd select the watched keys with the same conventions as Range:
	// an empty RangeEnd watches Key only, and RangeEnd "\x00" watches every key
	// at or above Key.
	Key      []byte
	RangeEnd []byte
	// StartRevision is the first revision to deliver. Zero means "after the current revision".
	StartRevision uint64
	// NoPut and NoDelete filter events of the respective type out.
	NoPut    bool
	NoDelete bool
	// PrevKV delivers the previous key-value pair with each event.
	PrevKV bool
}
