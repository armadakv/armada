// Copyright JAMF Software, LLC

package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test_dirSize(t *testing.T) {
	t.Run("non-existent directory returns 0", func(t *testing.T) {
		assert.Equal(t, int64(0), dirSize("/does/not/exist"))
	})

	t.Run("empty directory returns 0", func(t *testing.T) {
		dir := t.TempDir()
		assert.Equal(t, int64(0), dirSize(dir))
	})

	t.Run("sums regular file sizes recursively", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a"), make([]byte, 100), 0o600))
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.MkdirAll(sub, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "b"), make([]byte, 200), 0o600))

		assert.Equal(t, int64(300), dirSize(dir))
	})
}

func Test_diskMetrics_Collect(t *testing.T) {
	raftDir := t.TempDir()
	walDir := t.TempDir()
	tableDir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(raftDir, "raft.log"), make([]byte, 512), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(walDir, "wal.log"), make([]byte, 256), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(tableDir, "sst"), make([]byte, 1024), 0o600))

	d := newDiskMetrics(raftDir, walDir, tableDir)

	ch := make(chan prometheus.Metric, 10)
	d.Collect(ch)
	close(ch)

	values := map[string]float64{}
	for m := range ch {
		var pb dto.Metric
		require.NoError(t, m.Write(&pb))
		values[m.Desc().String()] = pb.GetGauge().GetValue()
	}

	assert.Len(t, values, 3)
	assert.InDelta(t, float64(512), values[d.raftDesc.String()], 0)
	assert.InDelta(t, float64(256), values[d.walDesc.String()], 0)
	assert.InDelta(t, float64(1024), values[d.tableDesc.String()], 0)
}

func Test_diskMetrics_Collect_walColocated(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "data"), make([]byte, 128), 0o600))

	d := newDiskMetrics(dir, "", dir)

	ch := make(chan prometheus.Metric, 10)
	d.Collect(ch)
	close(ch)

	var walValue float64
	for m := range ch {
		if m.Desc().String() == d.walDesc.String() {
			var pb dto.Metric
			require.NoError(t, m.Write(&pb))
			walValue = pb.GetGauge().GetValue()
		}
	}
	assert.InDelta(t, float64(0), walValue, 0, "WAL metric should be 0 when colocated")
}
