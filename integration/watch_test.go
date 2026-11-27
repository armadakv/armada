// Copyright Armada Contributors

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
)

// ── watch ────────────────────────────────────────────────────────────────────

// scenarioWatch checks the property the Watch API is built around: revisions
// are the same on every cluster. Watchers on the leader and on two follower
// nodes see the same events at the same revisions, and a watch resumes on a
// different follower node from a revision observed on the leader, replaying
// the history from that node's store.
func (e *Env) scenarioWatch(t *testing.T, ctx context.Context) {
	defer e.use(t)()
	e.seed(t, ctx)

	const writes = 200
	from := e.written
	watched := []*Node{e.Leader(), e.Followers.Nodes[0], e.Followers.Nodes[1]}
	streams := make([]armadapb.Watch_WatchClient, len(watched))
	for i, n := range watched {
		streams[i] = e.openWatch(t, ctx, n, 0)
	}

	e.Load(ctx, from, from+writes)

	results := make([][]*armadapb.Event, len(watched))
	for i, n := range watched {
		results[i] = e.recvEvents(t, n, streams[i], writes, 3*time.Minute)
		if err := checkWatchEvents(results[i], from, from+writes); err != nil {
			t.Fatalf("events on %s: %v", n.Name, err)
		}
	}
	for i := 1; i < len(watched); i++ {
		if err := sameEvents(results[0], results[i]); err != nil {
			t.Fatalf("%s and %s disagree: %v", watched[0].Name, watched[i].Name, err)
		}
	}
	infof("✓ leader and follower clusters delivered the same %d events at the same revisions", writes)

	// Resume on a node that has not watched yet, from a revision seen on the
	// leader: the follower replays the rest of the history from its store.
	resumeAt := results[0][writes/2].Kv.ModRevision
	resumed := e.Followers.Nodes[2]
	stream := e.openWatch(t, ctx, resumed, resumeAt)
	tail := e.recvEvents(t, resumed, stream, writes-writes/2, 3*time.Minute)
	if err := sameEvents(results[0][writes/2:], tail); err != nil {
		t.Fatalf("resumed watch on %s from revision %d: %v", resumed.Name, resumeAt, err)
	}
	infof("✓ watch resumed on %s from leader revision %d", resumed.Name, resumeAt)
}

// openWatch opens a stream on n and creates a prefix watcher on the loader's
// keys from start (zero for "now").
func (e *Env) openWatch(t *testing.T, ctx context.Context, n *Node, start int64) armadapb.Watch_WatchClient {
	t.Helper()
	stream, err := armadapb.NewWatchClient(n.Conn(t)).Watch(ctx)
	if err != nil {
		t.Fatalf("open watch on %s: %v", n.Name, err)
	}
	req := &armadapb.WatchCreateRequest{
		Table:         []byte(e.Table),
		Key:           []byte("key-"),
		RangeEnd:      []byte("key."),
		StartRevision: start,
	}
	if err := stream.Send(&armadapb.WatchRequest{RequestUnion: &armadapb.WatchRequest_CreateRequest{CreateRequest: req}}); err != nil {
		t.Fatalf("create watch on %s: %v", n.Name, err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatalf("watch on %s: %v", n.Name, err)
	}
	if !resp.GetCreated() || resp.GetCanceled() {
		t.Fatalf("watch on %s was not created: %s", n.Name, resp.GetCancelReason())
	}
	return stream
}

// recvEvents reads n events from a stream.
func (e *Env) recvEvents(t *testing.T, n *Node, stream armadapb.Watch_WatchClient, count int, timeout time.Duration) []*armadapb.Event {
	t.Helper()
	type result struct {
		events []*armadapb.Event
		err    error
	}
	done := make(chan result, 1)
	go func() {
		var events []*armadapb.Event
		for len(events) < count {
			resp, err := stream.Recv()
			if err != nil {
				done <- result{events, err}
				return
			}
			if resp.GetCanceled() {
				done <- result{events, fmt.Errorf("watch canceled: %s (compact revision %d)", resp.GetCancelReason(), resp.GetCompactRevision())}
				return
			}
			events = append(events, resp.GetEvents()...)
		}
		done <- result{events, nil}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("watch on %s after %d events: %v", n.Name, len(r.events), r.err)
		}
		return r.events
	case <-time.After(e.cfg.scale(timeout)):
		t.Fatalf("watch on %s: timed out waiting for %d events", n.Name, count)
		return nil
	}
}

// checkWatchEvents checks that events are one PUT per loader key in [from, to),
// in strictly increasing revision order.
func checkWatchEvents(events []*armadapb.Event, from, to int) error {
	if len(events) != to-from {
		return fmt.Errorf("got %d events, want %d", len(events), to-from)
	}
	seen := make(map[string]bool, len(events))
	var last int64
	for i, ev := range events {
		if ev.GetType() != armadapb.Event_PUT {
			return fmt.Errorf("event %d is a %s", i, ev.GetType())
		}
		if rev := ev.GetKv().GetModRevision(); rev <= last {
			return fmt.Errorf("event %d has revision %d after %d", i, rev, last)
		} else {
			last = rev
		}
		seen[string(ev.GetKv().GetKey())] = true
	}
	for i := from; i < to; i++ {
		if !seen[string(key(i))] {
			return fmt.Errorf("no event for %s", key(i))
		}
	}
	return nil
}

// sameEvents checks that two nodes reported the same changes at the same revisions.
func sameEvents(want, got []*armadapb.Event) error {
	if len(want) != len(got) {
		return fmt.Errorf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i].GetKv(), got[i].GetKv()
		if want[i].GetType() != got[i].GetType() || string(w.GetKey()) != string(g.GetKey()) ||
			string(w.GetValue()) != string(g.GetValue()) || w.GetModRevision() != g.GetModRevision() ||
			w.GetCreateRevision() != g.GetCreateRevision() {
			return fmt.Errorf("event %d: want %s %s@%d, got %s %s@%d", i,
				want[i].GetType(), w.GetKey(), w.GetModRevision(), got[i].GetType(), g.GetKey(), g.GetModRevision())
		}
	}
	return nil
}
