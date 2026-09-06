// Copyright 2017-2025 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func TestReadFrameUsesProtocolSpecificSizeLimit(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(maxSnapshotFrameSize+1))
	_, _, err := readFrame(bytes.NewReader(header[:]), maxSnapshotFrameSize,
		newFrameBudget(inboundFrameBudget), make(chan struct{}))
	if err == nil {
		t.Fatal("oversized snapshot frame was accepted")
	}

	binary.BigEndian.PutUint32(header[:], 8)
	payload := make([]byte, 8)
	data := append(header[:], payload...)
	frame, release, err := readFrame(bytes.NewReader(data), 8,
		newFrameBudget(inboundFrameBudget), make(chan struct{}))
	if err != nil {
		t.Fatalf("frame at its size limit was rejected: %v", err)
	}
	if len(frame) != len(payload) {
		t.Fatalf("got frame length %d, want %d", len(frame), len(payload))
	}
	release()
}

func TestFrameBudgetCancellationReleasesAcquiredCapacity(t *testing.T) {
	budget := newFrameBudget(frameBudgetUnit)
	stopc := make(chan struct{})
	release, err := budget.acquire(uint32(frameBudgetUnit), stopc)
	if err != nil {
		t.Fatalf("failed to acquire initial frame budget: %v", err)
	}
	close(stopc)
	if _, err := budget.acquire(1, stopc); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
	release()
	if _, err := budget.acquire(uint32(frameBudgetUnit), make(chan struct{})); err != nil {
		t.Fatalf("frame budget capacity was not released: %v", err)
	}
}
