// Copyright JAMF Software, LLC

package pebble

import (
	"sync"
	"testing"
	"time"

	"github.com/benbjohnson/clock"
	cpebble "github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
)

type schedulerTestDB struct {
	mu        sync.Mutex
	allowed   int
	waiting   *cpebble.WaitingCompaction
	scheduled chan cpebble.CompactionGrantHandle
}

func (d *schedulerTestDB) GetAllowedWithoutPermission() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.allowed
}

func (d *schedulerTestDB) GetWaitingCompaction() (bool, cpebble.WaitingCompaction) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waiting == nil {
		return false, cpebble.WaitingCompaction{}
	}
	return true, *d.waiting
}

func (d *schedulerTestDB) Schedule(handle cpebble.CompactionGrantHandle) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.waiting == nil {
		return false
	}
	d.waiting = nil
	d.scheduled <- handle
	return true
}

func TestConcurrencyLimitSchedulerCoordinatesDBs(t *testing.T) {
	coordinator := newConcurrencyLimitScheduler(clock.NewMock())
	first := coordinator.NewScheduler().(*schedulerRegistration)
	second := coordinator.NewScheduler().(*schedulerRegistration)
	firstDB := &schedulerTestDB{allowed: 1, scheduled: make(chan cpebble.CompactionGrantHandle, 1)}
	secondDB := &schedulerTestDB{
		allowed:   1,
		waiting:   &cpebble.WaitingCompaction{Priority: 1, Score: 1},
		scheduled: make(chan cpebble.CompactionGrantHandle, 1),
	}
	first.Register(1, firstDB)
	second.Register(1, secondDB)
	defer first.Unregister()
	defer second.Unregister()

	granted, firstHandle := first.TrySchedule()
	require.True(t, granted)
	require.NotNil(t, firstHandle)

	granted, secondHandle := second.TrySchedule()
	require.False(t, granted)
	require.Nil(t, secondHandle)

	firstHandle.Done()
	grantedHandle := waitForScheduled(t, secondDB.scheduled)
	require.NotNil(t, grantedHandle)

	coordinator.mu.Lock()
	require.Equal(t, 1, coordinator.mu.runningCompactions)
	require.Equal(t, 0, first.runningCompactions)
	require.Equal(t, 1, second.runningCompactions)
	coordinator.mu.Unlock()

	grantedHandle.Done()
}

func TestConcurrencyLimitSchedulerPrioritizesAcrossDBs(t *testing.T) {
	coordinator := newConcurrencyLimitScheduler(clock.NewMock())
	first := coordinator.NewScheduler().(*schedulerRegistration)
	second := coordinator.NewScheduler().(*schedulerRegistration)
	firstDB := &schedulerTestDB{
		allowed:   1,
		waiting:   &cpebble.WaitingCompaction{Optional: true, Priority: 100, Score: 100},
		scheduled: make(chan cpebble.CompactionGrantHandle, 1),
	}
	secondDB := &schedulerTestDB{
		allowed:   1,
		waiting:   &cpebble.WaitingCompaction{Priority: 1, Score: 1},
		scheduled: make(chan cpebble.CompactionGrantHandle, 1),
	}
	first.Register(1, firstDB)
	second.Register(1, secondDB)
	defer first.Unregister()
	defer second.Unregister()

	first.markWaitingForTest()
	second.markWaitingForTest()
	coordinator.tryGrant()

	secondHandle := waitForScheduled(t, secondDB.scheduled)
	select {
	case <-firstDB.scheduled:
		t.Fatal("optional compaction was scheduled before required work")
	default:
	}
	secondHandle.Done()
	firstHandle := waitForScheduled(t, firstDB.scheduled)
	firstHandle.Done()
}

func waitForScheduled(t *testing.T, scheduled <-chan cpebble.CompactionGrantHandle) cpebble.CompactionGrantHandle {
	t.Helper()
	select {
	case handle := <-scheduled:
		return handle
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for scheduled compaction")
		return nil
	}
}

func (r *schedulerRegistration) markWaitingForTest() {
	r.coordinator.mu.Lock()
	defer r.coordinator.mu.Unlock()
	r.markWaitingLocked()
}
