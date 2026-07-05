package store

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectBestSnapshot(t *testing.T) {
	metas := []Meta{
		{Table: "t", Type: SnapshotTypeFull, BaseIndex: 0, TipIndex: 100},
		{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 100, TipIndex: 140},
		{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 140, TipIndex: 180},
	}

	t.Run("picks nearest incremental", func(t *testing.T) {
		got, ok := SelectBestSnapshot(metas, 120)
		require.True(t, ok)
		require.Equal(t, SnapshotTypeIncremental, got.Type)
		require.Equal(t, uint64(100), got.BaseIndex)
		require.Equal(t, uint64(140), got.TipIndex)
	})

	t.Run("falls back to full when follower too far behind", func(t *testing.T) {
		got, ok := SelectBestSnapshot(metas, 50)
		require.True(t, ok)
		require.Equal(t, SnapshotTypeFull, got.Type)
		require.Equal(t, uint64(100), got.TipIndex)
	})

	t.Run("returns none when follower already up to date", func(t *testing.T) {
		_, ok := SelectBestSnapshot(metas, 180)
		require.False(t, ok)
	})
}
