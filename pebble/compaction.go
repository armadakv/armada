// Copyright JAMF Software, LLC

package pebble

import (
	"sync"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/cockroachdb/pebble/v2"
)

// ConcurrencyLimitScheduler coordinates compaction scheduling across all
// registered Pebble DBs. Each DB must use a distinct schedulerRegistration,
// created by NewScheduler, because Pebble's CompactionScheduler callbacks do
// not identify the calling DB.
type ConcurrencyLimitScheduler struct {
	ts clock.Clock
	mu struct {
		sync.Mutex
		registrations      map[*schedulerRegistration]struct{}
		runningCompactions int
		isGranting         bool
		isGrantingCond     *sync.Cond
		backgroundRunning  bool
	}
	pokeBackgroundGranterCh chan struct{}
}

type schedulerRegistration struct {
	coordinator *ConcurrencyLimitScheduler
	db          pebble.DBForCompaction

	// The fields below are protected by coordinator.mu.
	registered                   bool
	runningCompactions           int
	waiting                      bool
	waitingGeneration            uint64
	lastAllowedWithoutPermission int
}

var (
	_ pebble.CompactionScheduler   = (*schedulerRegistration)(nil)
	_ pebble.CompactionGrantHandle = (*schedulerRegistration)(nil)
)

func NewConcurrencyLimitScheduler() *ConcurrencyLimitScheduler {
	return newConcurrencyLimitScheduler(clock.New())
}

func newConcurrencyLimitScheduler(ts clock.Clock) *ConcurrencyLimitScheduler {
	s := &ConcurrencyLimitScheduler{
		ts:                      ts,
		pokeBackgroundGranterCh: make(chan struct{}, 1),
	}
	s.mu.registrations = make(map[*schedulerRegistration]struct{})
	s.mu.isGrantingCond = sync.NewCond(&s.mu.Mutex)
	return s
}

// NewScheduler returns a single-use scheduler registration for one Pebble DB.
// The returned value must not be reused by another DB after it unregisters.
func (s *ConcurrencyLimitScheduler) NewScheduler() pebble.CompactionScheduler {
	return &schedulerRegistration{coordinator: s}
}

func (r *schedulerRegistration) Register(_ int, db pebble.DBForCompaction) {
	s := r.coordinator
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.registered || r.db != nil {
		panic("compaction scheduler registration reused")
	}
	// Grant selection releases the coordinator mutex while iterating over the
	// registrations, so keep the registration set stable for the whole pass.
	for s.mu.isGranting {
		s.mu.isGrantingCond.Wait()
	}
	r.db = db
	r.registered = true
	s.mu.registrations[r] = struct{}{}
	if !s.mu.backgroundRunning {
		s.mu.backgroundRunning = true
		go s.backgroundGranter()
	}
}

func (r *schedulerRegistration) Unregister() {
	s := r.coordinator
	s.mu.Lock()
	if !r.registered {
		s.mu.Unlock()
		panic("compaction scheduler was not registered")
	}
	// Keep the registration stable while a grant pass may call into its DB.
	for s.mu.isGranting {
		s.mu.isGrantingCond.Wait()
	}
	r.registered = false
	delete(s.mu.registrations, r)
	s.mu.Unlock()
	s.pokeBackgroundGranter()
}

func (r *schedulerRegistration) TrySchedule() (bool, pebble.CompactionGrantHandle) {
	s := r.coordinator
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.registered {
		return false, nil
	}
	if s.mu.isGranting {
		r.markWaitingLocked()
		s.pokeBackgroundGranter()
		return false, nil
	}

	r.lastAllowedWithoutPermission = r.db.GetAllowedWithoutPermission()
	if r.runningCompactions < r.lastAllowedWithoutPermission &&
		s.mu.runningCompactions < s.globalLimitLocked() {
		r.runningCompactions++
		s.mu.runningCompactions++
		return true, r
	}
	r.markWaitingLocked()
	return false, nil
}

func (r *schedulerRegistration) UpdateGetAllowedWithoutPermission() {
	s := r.coordinator
	s.mu.Lock()
	if !r.registered {
		s.mu.Unlock()
		return
	}
	allowedWithoutPermission := r.db.GetAllowedWithoutPermission()
	tryGrant := allowedWithoutPermission > r.lastAllowedWithoutPermission
	r.lastAllowedWithoutPermission = allowedWithoutPermission
	s.mu.Unlock()
	if tryGrant {
		s.pokeBackgroundGranter()
	}
}

func (r *schedulerRegistration) Started()                                          {}
func (r *schedulerRegistration) MeasureCPU(pebble.CompactionGoroutineKind)         {}
func (r *schedulerRegistration) CumulativeStats(pebble.CompactionGrantHandleStats) {}

func (r *schedulerRegistration) Done() {
	s := r.coordinator
	s.mu.Lock()
	r.runningCompactions--
	s.mu.runningCompactions--
	s.mu.Unlock()
	s.pokeBackgroundGranter()
}

func (r *schedulerRegistration) markWaitingLocked() {
	r.waiting = true
	r.waitingGeneration++
}

func (s *ConcurrencyLimitScheduler) pokeBackgroundGranter() {
	select {
	case s.pokeBackgroundGranterCh <- struct{}{}:
	default:
	}
}

// globalLimitLocked returns the node-wide compaction limit. Pebble computes a
// dynamic soft limit per DB from compaction debt. Using the maximum of those
// limits prevents the number of concurrent compactions from multiplying with
// the number of table DBs while still allowing a DB with significant debt to
// request additional concurrency.
func (s *ConcurrencyLimitScheduler) globalLimitLocked() int {
	limit := 0
	for r := range s.mu.registrations {
		allowed := r.db.GetAllowedWithoutPermission()
		r.lastAllowedWithoutPermission = allowed
		if allowed > limit {
			limit = allowed
		}
	}
	return limit
}

type compactionCandidate struct {
	registration *schedulerRegistration
	generation   uint64
	waiting      pebble.WaitingCompaction
}

func (s *ConcurrencyLimitScheduler) tryGrant() {
	s.mu.Lock()
	if s.mu.isGranting {
		s.mu.Unlock()
		return
	}
	s.mu.isGranting = true
	defer func() {
		s.mu.isGranting = false
		s.mu.isGrantingCond.Broadcast()
		s.mu.Unlock()
	}()

	for s.mu.runningCompactions < s.globalLimitLocked() {
		candidate, ok := s.bestCandidateLocked()
		if !ok {
			return
		}

		s.mu.Unlock()
		accepted := candidate.registration.db.Schedule(candidate.registration)
		s.mu.Lock()
		if accepted {
			candidate.registration.runningCompactions++
			s.mu.runningCompactions++
			continue
		}
		if candidate.registration.waitingGeneration == candidate.generation {
			candidate.registration.waiting = false
		}
	}
}

// bestCandidateLocked returns the highest-priority waiting compaction while
// preserving Pebble's ordering: required before optional, then higher priority,
// then higher score.
func (s *ConcurrencyLimitScheduler) bestCandidateLocked() (compactionCandidate, bool) {
	var best compactionCandidate
	found := false
	for r := range s.mu.registrations {
		if !r.waiting || r.runningCompactions >= r.lastAllowedWithoutPermission {
			continue
		}
		generation := r.waitingGeneration
		s.mu.Unlock()
		waiting, info := r.db.GetWaitingCompaction()
		s.mu.Lock()
		if !waiting {
			if r.waitingGeneration == generation {
				r.waiting = false
			}
			continue
		}
		candidate := compactionCandidate{registration: r, generation: generation, waiting: info}
		if !found || higherPriority(candidate.waiting, best.waiting) {
			best = candidate
			found = true
		}
	}
	return best, found
}

func higherPriority(a, b pebble.WaitingCompaction) bool {
	if a.Optional != b.Optional {
		return !a.Optional
	}
	if a.Priority != b.Priority {
		return a.Priority > b.Priority
	}
	return a.Score > b.Score
}

func (s *ConcurrencyLimitScheduler) backgroundGranter() {
	ticker := s.ts.Ticker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.pokeBackgroundGranterCh:
		}
		s.tryGrant()

		s.mu.Lock()
		if len(s.mu.registrations) == 0 {
			s.mu.backgroundRunning = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
	}
}
