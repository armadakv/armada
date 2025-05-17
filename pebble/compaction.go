// Copyright 2024 - 2025 Jakub Coufal (coufalja@gmail.com) and other contributors.

package pebble

import (
	"sync"
	"time"

	"github.com/benbjohnson/clock"

	"github.com/cockroachdb/pebble"
)

type ConcurrencyLimitScheduler struct {
	ts clock.Clock
	// db is set in Register, but not protected by mu since it is strictly
	// before any calls to the other methods.
	db pebble.DBForCompaction
	mu struct {
		sync.Mutex
		runningCompactions           int
		unregistered                 bool
		isGranting                   bool
		isGrantingCond               *sync.Cond
		lastAllowedWithoutPermission int
	}
	stopPeriodicGranterCh chan struct{}
	pokePeriodicGranterCh chan struct{}
}

func NewConcurrencyLimitScheduler() *ConcurrencyLimitScheduler {
	s := &ConcurrencyLimitScheduler{
		ts:                    clock.New(),
		stopPeriodicGranterCh: make(chan struct{}),
		pokePeriodicGranterCh: make(chan struct{}, 1),
	}
	s.mu.isGrantingCond = sync.NewCond(&s.mu.Mutex)
	return s
}

func (s *ConcurrencyLimitScheduler) Register(numGoroutinesPerCompaction int, db pebble.DBForCompaction) {
	s.db = db
	if s.stopPeriodicGranterCh != nil {
		go s.periodicGranter()
	}
}

func (s *ConcurrencyLimitScheduler) Unregister() {
	if s.stopPeriodicGranterCh != nil {
		s.stopPeriodicGranterCh <- struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mu.unregistered = true
	for s.mu.isGranting {
		s.mu.isGrantingCond.Wait()
	}
}

func (s *ConcurrencyLimitScheduler) TrySchedule() (bool, pebble.CompactionGrantHandle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mu.unregistered {
		return false, nil
	}
	s.mu.lastAllowedWithoutPermission = s.db.GetAllowedWithoutPermission()
	if s.mu.lastAllowedWithoutPermission > s.mu.runningCompactions {
		s.mu.runningCompactions++
		return true, s
	}
	return false, nil
}

func (s *ConcurrencyLimitScheduler) Started()                                                {}
func (s *ConcurrencyLimitScheduler) MeasureCPU(pebble.CompactionGoroutineKind)               {}
func (s *ConcurrencyLimitScheduler) CumulativeStats(stats pebble.CompactionGrantHandleStats) {}

func (s *ConcurrencyLimitScheduler) Done() {
	s.mu.Lock()
	s.mu.runningCompactions--
	s.tryGrantLockedAndUnlock()
}

func (s *ConcurrencyLimitScheduler) UpdateGetAllowedWithoutPermission() {
	s.mu.Lock()
	allowedWithoutPermission := s.db.GetAllowedWithoutPermission()
	tryGrant := allowedWithoutPermission > s.mu.lastAllowedWithoutPermission
	s.mu.lastAllowedWithoutPermission = allowedWithoutPermission
	s.mu.Unlock()
	if tryGrant {
		select {
		case s.pokePeriodicGranterCh <- struct{}{}:
		default:
		}
	}
}

func (s *ConcurrencyLimitScheduler) tryGrantLockedAndUnlock() {
	defer s.mu.Unlock()
	if s.mu.unregistered {
		return
	}
	// Wait for turn to grant.
	for s.mu.isGranting {
		s.mu.isGrantingCond.Wait()
	}
	// INVARIANT: !isGranting.
	if s.mu.unregistered {
		return
	}
	s.mu.lastAllowedWithoutPermission = s.db.GetAllowedWithoutPermission()
	toGrant := s.mu.lastAllowedWithoutPermission - s.mu.runningCompactions
	if toGrant > 0 {
		s.mu.isGranting = true
	} else {
		return
	}
	s.mu.Unlock()
	// We call GetWaitingCompaction iff we can successfully grant, so that there
	// is no wasted pickedCompaction.
	//
	// INVARIANT: loop exits with s.mu unlocked.
	for toGrant > 0 {
		waiting, _ := s.db.GetWaitingCompaction()
		if !waiting {
			break
		}
		accepted := s.db.Schedule(s)
		if !accepted {
			break
		}
		s.mu.Lock()
		s.mu.runningCompactions++
		toGrant--
		s.mu.Unlock()
	}
	// Will be unlocked by the defer statement.
	s.mu.Lock()
	s.mu.isGranting = false
	s.mu.isGrantingCond.Broadcast()
}

func (s *ConcurrencyLimitScheduler) periodicGranter() {
	ticker := s.ts.Ticker(100 * time.Millisecond)
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.tryGrantLockedAndUnlock()
		case <-s.pokePeriodicGranterCh:
			s.mu.Lock()
			s.tryGrantLockedAndUnlock()
		case <-s.stopPeriodicGranterCh:
			ticker.Stop()
			return
		}
	}
}
