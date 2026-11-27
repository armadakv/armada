// Copyright Armada Contributors

package fsm

import (
	"bytes"
	"cmp"
	"slices"

	"github.com/armadakv/armada/armadapb"
)

// Observer is notified about committed apply batches. It is how the Watch API
// learns about changes to a table. Implementations must not block: they are
// called on the Raft apply path.
type Observer interface {
	// Capturing reports whether the next apply batch should record its events.
	// It is called once per batch, so it must be cheap.
	Capturing() bool
	// Committed is called after a batch has been committed to Pebble, so that
	// any read started afterwards observes it. It is also called with
	// Captured == false when the state machine opens or recovers from a Raft
	// snapshot, to report the revision it now holds.
	Committed(Commit)
	// Closed is called once the state machine is closed.
	Closed()
}

// Commit describes the effect of one committed apply batch.
type Commit struct {
	// Revision is the table revision after the batch: the source sequence of
	// the last applied write. It is the same value on every cluster.
	Revision uint64
	// Captured reports whether Events holds every change of the batch. When it
	// is false the changes up to Revision were not recorded and have to be read
	// back from the store.
	Captured bool
	// Events are the changes of the batch, ordered by modification revision
	// and then by key. There is at most one event per key and revision.
	Events []*armadapb.Event
	// Rewritten reports that the batch replaced part of the table's history
	// instead of extending it: a snapshot replay that writes records at their
	// source revisions, or a reset of the revision. Events are not recorded
	// for such writes, and a watcher cannot be continued across them.
	Rewritten bool
}

type noopObserver struct{}

func (noopObserver) Capturing() bool  { return false }
func (noopObserver) Committed(Commit) {}
func (noopObserver) Closed()          {}

type eventKey struct {
	rev uint64
	key string
}

// eventRecorder collects the events of one apply batch. It keeps one event per
// key and revision, matching the single physical version the store keeps for
// a (key, modification revision) pair: a later write to the same key at the
// same revision replaces the earlier event but keeps its previous key-value,
// which is the state before the revision.
type eventRecorder struct {
	events []*armadapb.Event
	index  map[eventKey]int
}

func newEventRecorder() *eventRecorder {
	return &eventRecorder{index: make(map[eventKey]int)}
}

func (r *eventRecorder) record(ev *armadapb.Event) {
	k := eventKey{rev: uint64(ev.Kv.ModRevision), key: string(ev.Kv.Key)}
	if i, ok := r.index[k]; ok {
		ev.PrevKv = r.events[i].PrevKv
		r.events[i] = ev
		return
	}
	r.index[k] = len(r.events)
	r.events = append(r.events, ev)
}

// sorted returns the recorded events ordered by revision and key.
func (r *eventRecorder) sorted() []*armadapb.Event {
	slices.SortFunc(r.events, func(a, b *armadapb.Event) int {
		if c := cmp.Compare(a.Kv.ModRevision, b.Kv.ModRevision); c != 0 {
			return c
		}
		return bytes.Compare(a.Kv.Key, b.Kv.Key)
	})
	return r.events
}

// newEvent builds an event, copying all byte slices: commands are decoded in
// place from Raft entry memory that is reused once the batch is applied.
func newEvent(typ armadapb.Event_EventType, userKey, value []byte, createSeqno, modSeqno uint64, prev *armadapb.KeyValue) *armadapb.Event {
	kv := &armadapb.KeyValue{
		Key:            bytes.Clone(userKey),
		CreateRevision: int64(createSeqno),
		ModRevision:    int64(modSeqno),
	}
	if typ == armadapb.Event_PUT {
		kv.Value = bytes.Clone(value)
	}
	return &armadapb.Event{Type: typ, Kv: kv, PrevKv: prev}
}

// sequenceRevision returns the source sequence of a SEQUENCE command. Live
// replication sets the outer index to its last child. Snapshot replay leaves
// it unset and orders its children by key, so the highest child is used.
func sequenceRevision(cmd *armadapb.Command) uint64 {
	if cmd.LeaderIndex != nil {
		return *cmd.LeaderIndex
	}
	var r uint64
	for _, child := range cmd.Sequence {
		switch {
		case child.LeaderIndex != nil:
			r = max(r, *child.LeaderIndex)
		case child.Type == armadapb.Command_SEQUENCE:
			r = max(r, sequenceRevision(child))
		}
	}
	return r
}
