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
	"fmt"

	serrors "github.com/armadakv/armada/storage/errors"
)

var (
	// ErrNotServed is returned when the table's serving shard does not run on this node.
	ErrNotServed = errors.New("table is not served by this node")
	// ErrClosed is returned once the registry is closed.
	ErrClosed = errors.New("watch registry is closed")
)

// CompactedError is returned for a start revision whose events are no longer
// available. It matches serrors.ErrCompacted.
type CompactedError struct {
	// CompactRevision is the first available revision.
	CompactRevision uint64
}

func (e *CompactedError) Error() string {
	return fmt.Sprintf("required revision has been compacted, the first available revision is %d", e.CompactRevision)
}

func (e *CompactedError) Unwrap() error {
	return serrors.ErrCompacted
}

// Cancel reasons reported to clients.
const (
	reasonRewritten = "table history was rewritten by a recovery or a reset, re-list and watch again"
	reasonCompacted = "required revision has been compacted, re-list and watch again"
	reasonClosed    = "table is no longer served by this node"
	reasonDeleted   = "table was deleted"
	reasonReplaced  = "table was recreated or replaced by a recovery, re-list and watch again"
	reasonShutdown  = "node is shutting down"
	// ReasonResponseTooLarge is used by the server for a revision that does
	// not fit one response when the watcher did not allow fragments.
	ReasonResponseTooLarge = "a revision exceeds the maximum response size, watch with fragment enabled"
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
	// ProgressNotify asks for periodic progress notifications while the
	// watcher receives no events.
	ProgressNotify bool
}
