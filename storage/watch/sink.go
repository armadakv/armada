// Copyright Armada Contributors

package watch

import (
	"context"
	"errors"
	"sync"

	"github.com/armadakv/armada/armadapb"
)

// DefaultSinkLimit is the default number of event bytes a sink queues before
// its watchers are considered too slow.
const DefaultSinkLimit = 16 * 1024 * 1024

// ErrSinkClosed is returned by Sink.Next once the sink is closed.
var ErrSinkClosed = errors.New("watch sink is closed")

// Message is one response for a watcher.
type Message struct {
	// WatchID is the ID of the watcher the message belongs to.
	WatchID int64
	// ShardID is the shard the watcher is registered on; zero when the
	// watcher was never registered.
	ShardID uint64
	// Revision is the table revision the message reflects.
	Revision uint64
	// Events are the events, in revision order.
	Events []*armadapb.Event
	// Created marks the response to a create request.
	Created bool
	// Canceled marks the last message of a watcher.
	Canceled bool
	// CompactRevision is the first available revision, set when the watcher
	// was canceled because the revisions it needs are no longer available.
	CompactRevision uint64
	// Reason explains a cancellation.
	Reason string
}

// Sink queues the messages of all watchers of one stream in order. Event
// messages are refused once the queued event bytes reach the limit, which is
// how a slow stream is detected; control messages are always accepted.
type Sink struct {
	mu     sync.Mutex
	queue  []Message
	size   int
	limit  int
	ready  chan struct{}
	room   chan struct{}
	closed bool
}

// NewSink returns a sink queueing at most limit event bytes.
func NewSink(limit int) *Sink {
	if limit <= 0 {
		limit = DefaultSinkLimit
	}
	return &Sink{limit: limit, ready: make(chan struct{}, 1), room: make(chan struct{}, 1)}
}

// offer queues an event message of the given size unless the sink is full.
// A single message larger than the limit is accepted when no event bytes are
// queued, so that a large revision can always make progress.
func (s *Sink) offer(m Message, size int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	if s.size > 0 && s.size+size > s.limit {
		return false
	}
	s.queue = append(s.queue, m)
	s.size += size
	s.signal()
	return true
}

// Push queues a control message.
func (s *Sink) Push(m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.queue = append(s.queue, m)
	s.signal()
}

// waitRoom waits until no event bytes are queued. It returns false when the
// sink closes or stop is closed first.
func (s *Sink) waitRoom(stop <-chan struct{}) bool {
	for {
		s.mu.Lock()
		closed, empty := s.closed, s.size == 0
		s.mu.Unlock()
		switch {
		case closed:
			return false
		case empty:
			return true
		}
		select {
		case <-stop:
			return false
		case <-s.room:
		}
	}
}

func (s *Sink) signal() {
	select {
	case s.ready <- struct{}{}:
	default:
	}
}

// Next waits for queued messages and returns all of them.
func (s *Sink) Next(ctx context.Context) ([]Message, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			q := s.queue
			s.queue = nil
			s.size = 0
			s.mu.Unlock()
			select {
			case s.room <- struct{}{}:
			default:
			}
			return q, nil
		}
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, ErrSinkClosed
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.ready:
		}
	}
}

// Close discards queued messages and refuses new ones.
func (s *Sink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.queue = nil
	s.size = 0
	s.signal()
	select {
	case s.room <- struct{}{}:
	default:
	}
}

func eventsSize(events []*armadapb.Event) int {
	n := 0
	for _, ev := range events {
		n += ev.SizeVT()
	}
	return n
}
