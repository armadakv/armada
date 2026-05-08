// Copyright JAMF Software, LLC

package storage

import (
	"io/fs"
	"path/filepath"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	raftDiskBytesMetricName  = "armada_storage_raft_disk_bytes"
	walDiskBytesMetricName   = "armada_storage_wal_disk_bytes"
	tableDiskBytesMetricName = "armada_storage_table_disk_bytes"
)

// diskMetrics is a prometheus.Collector that reports the on-disk footprint of
// the three configurable storage directories:
//   - NodeHostDir  – Raft log and Dragonboat metadata
//   - WALDir       – separate WAL directory (optional; 0 when colocated)
//   - Table.DataDir – per-table Pebble state-machine data
//
// Sizes are computed by walking the directory tree on every Prometheus scrape.
// Because scrapes are infrequent (typically every 15–60 s) the walk overhead
// is acceptable; a per-table breakdown can be added as a follow-up.
type diskMetrics struct {
	nodeHostDir string
	walDir      string
	tableDir    string

	raftDesc  *prometheus.Desc
	walDesc   *prometheus.Desc
	tableDesc *prometheus.Desc
}

func newDiskMetrics(nodeHostDir, walDir, tableDir string) *diskMetrics {
	return &diskMetrics{
		nodeHostDir: nodeHostDir,
		walDir:      walDir,
		tableDir:    tableDir,
		raftDesc: prometheus.NewDesc(
			raftDiskBytesMetricName,
			"Disk space used by the Raft log (NodeHostDir) in bytes.",
			nil, nil,
		),
		walDesc: prometheus.NewDesc(
			walDiskBytesMetricName,
			"Disk space used by the WAL directory in bytes; 0 when WAL is colocated with the Raft log.",
			nil, nil,
		),
		tableDesc: prometheus.NewDesc(
			tableDiskBytesMetricName,
			"Disk space used by the state-machine (Pebble) directory in bytes.",
			nil, nil,
		),
	}
}

func (d *diskMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- d.raftDesc
	ch <- d.walDesc
	ch <- d.tableDesc
}

func (d *diskMetrics) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.MustNewConstMetric(d.raftDesc, prometheus.GaugeValue, float64(dirSize(d.nodeHostDir)))

	walSize := 0.0
	if d.walDir != "" && d.walDir != d.nodeHostDir {
		walSize = float64(dirSize(d.walDir))
	}
	ch <- prometheus.MustNewConstMetric(d.walDesc, prometheus.GaugeValue, walSize)

	ch <- prometheus.MustNewConstMetric(d.tableDesc, prometheus.GaugeValue, float64(dirSize(d.tableDir)))
}

// dirSize returns the total byte count of all regular files found recursively
// under path. Returns 0 if the directory does not exist yet (e.g. before first
// write) or if any entry cannot be stat'd.
func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries gracefully
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
