// Copyright Armada Contributors

package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectRecoverableSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		metas   []Meta
		options SnapshotSelectionOptions
		want    Meta
		found   bool
	}{
		{
			name: "invalid highest ranked incremental leaves valid lower ranked incremental",
			metas: []Meta{
				{Table: "orders", Type: SnapshotTypeIncremental, BaseIndex: 120, TipIndex: 180, GCHorizon: 120},
				{Table: "orders", Type: SnapshotTypeIncremental, BaseIndex: 100, TipIndex: 160, GCHorizon: 90},
			},
			options: SnapshotSelectionOptions{FollowerIndex: 125, LiveGCHorizon: 100, RequireKnownIncrementalHorizon: true},
			want:    Meta{Table: "orders", Type: SnapshotTypeIncremental, BaseIndex: 100, TipIndex: 160, GCHorizon: 90},
			found:   true,
		},
		{
			name: "invalid incremental falls back to valid full",
			metas: []Meta{
				{Table: "orders", Type: SnapshotTypeIncremental, BaseIndex: 100, TipIndex: 150, GCHorizon: 100},
				{Table: "orders", Type: SnapshotTypeFull, TipIndex: 200},
			},
			options: SnapshotSelectionOptions{FollowerIndex: 120, LiveGCHorizon: 100, RequireKnownIncrementalHorizon: true},
			want:    Meta{Table: "orders", Type: SnapshotTypeFull, TipIndex: 200},
			found:   true,
		},
		{
			name:    "no applicable artefact",
			metas:   []Meta{{Table: "orders", Type: SnapshotTypeIncremental, BaseIndex: 130, TipIndex: 160, GCHorizon: 100}},
			options: SnapshotSelectionOptions{FollowerIndex: 120, LiveGCHorizon: 100, RequireKnownIncrementalHorizon: true},
			found:   false,
		},
		{
			name:    "tip below horizon is rejected",
			metas:   []Meta{{Table: "orders", Type: SnapshotTypeFull, TipIndex: 99}},
			options: SnapshotSelectionOptions{FollowerIndex: 10, LiveGCHorizon: 100},
			found:   false,
		},
		{
			name:    "follower able to tail has no selection",
			metas:   []Meta{{Table: "orders", Type: SnapshotTypeFull, TipIndex: 200}},
			options: SnapshotSelectionOptions{FollowerIndex: 100, LiveGCHorizon: 100, FollowerCanTail: true},
			found:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SelectRecoverableSnapshot(tt.metas, tt.options)
			require.Equal(t, tt.found, ok)
			if ok {
				require.Equal(t, tt.want, got)
			}
		})
	}
}
