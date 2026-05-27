// Copyright JAMF Software, LLC

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage/table"
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
	"go.uber.org/zap"
)

// fakeTableService implements tableProvider for use in tests.
type fakeTableService struct {
	tableNames []string
	// index returned by Snapshot (simulates the applied leader index)
	snapshotIdx uint64
	// index returned by IncrementalSnapshot
	incrIdx uint64
	// data to write during Snapshot / IncrementalSnapshot
	snapshotData []byte
}

func (f *fakeTableService) GetTables() ([]table.Table, error) {
	tables := make([]table.Table, len(f.tableNames))
	for i, name := range f.tableNames {
		tables[i] = table.Table{Name: name}
	}
	return tables, nil
}

func (f *fakeTableService) GetTable(name string) (SnapshotTable, error) {
	for _, n := range f.tableNames {
		if n == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("table %q not found", name)
}

func (f *fakeTableService) Snapshot(_ context.Context, w io.Writer) (*fsm.SnapshotResponse, error) {
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return nil, err
		}
	}
	return &fsm.SnapshotResponse{Index: f.snapshotIdx}, nil
}

func (f *fakeTableService) IncrementalSnapshot(_ context.Context, w io.Writer, _ uint64) (*fsm.SnapshotResponse, error) {
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return nil, err
		}
	}
	return &fsm.SnapshotResponse{Index: f.incrIdx}, nil
}

// makeSnapshotData returns a minimal armadapb.Command serialized via the
// replication/snapshot snappy+length-prefix format so the exporter temp file
// is non-empty and well-formed.
func makeSnapshotData(t *testing.T) []byte {
	t.Helper()
	// The snapshot writer expects raw proto bytes (not yet length-prefixed).
	// We simulate what table.Snapshot() produces: raw marshaled Commands.
	cmd := &armadapb.Command{
		Table: []byte("test"),
		Type:  armadapb.Command_PUT,
		Kv:    &armadapb.KeyValue{Key: []byte("k"), Value: []byte("v")},
	}
	b, err := cmd.MarshalVT()
	require.NoError(t, err)
	return b
}

func newTestLogger() *zap.SugaredLogger {
	l, _ := zap.NewDevelopment()
	return l.Sugar()
}

// TestExportFull_Basic verifies that ExportFull uploads a .snap and .meta file.
func TestExportFull_Basic(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"orders"},
		snapshotIdx:  42,
		snapshotData: makeSnapshotData(t),
	}

	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "leader-1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}

	ctx := context.Background()
	require.NoError(t, exp.ExportFull(ctx, "orders"))

	snapKey := FullSnapKey("orders", 42)
	metaKey := FullMetaKey("orders", 42)

	// Both artefact and meta must exist.
	ok, err := bucket.Exists(ctx, snapKey)
	require.NoError(t, err)
	assert.True(t, ok, "snap key should exist")

	ok, err = bucket.Exists(ctx, metaKey)
	require.NoError(t, err)
	assert.True(t, ok, "meta key should exist")

	// Parse and validate the meta.
	r, err := bucket.Get(ctx, metaKey)
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)

	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))

	assert.Equal(t, "orders", m.Table)
	assert.Equal(t, SnapshotTypeFull, m.Type)
	assert.Equal(t, uint64(0), m.BaseIndex)
	assert.Equal(t, uint64(42), m.TipIndex)
	assert.Equal(t, "leader-1", m.NodeID)
	assert.Equal(t, SnapshotFormat, m.Format)
	assert.NotEmpty(t, m.SHA256)
	assert.Greater(t, m.SizeBytes, int64(0))
}

// TestExportFull_Idempotent verifies that calling ExportFull twice with the same
// tip index does not overwrite the artefact and returns no error.
func TestExportFull_Idempotent(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"orders"},
		snapshotIdx:  100,
		snapshotData: makeSnapshotData(t),
	}

	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "leader-1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}

	ctx := context.Background()
	require.NoError(t, exp.ExportFull(ctx, "orders"))

	// Count objects before second call.
	before := len(bucket.Objects())
	require.NoError(t, exp.ExportFull(ctx, "orders"))
	after := len(bucket.Objects())

	assert.Equal(t, before, after, "second ExportFull should not add new objects")
}

// TestExportFull_MultipleTablesAddsAllMeta verifies full export writes meta
// for multiple tables.
func TestExportFull_MultipleTablesAddsAllMeta(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"t1", "t2", "t3"},
		snapshotIdx:  10,
		snapshotData: makeSnapshotData(t),
	}

	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}

	ctx := context.Background()
	for _, tbl := range []string{"t1", "t2", "t3"} {
		require.NoError(t, exp.ExportFull(ctx, tbl))
	}

	for _, tbl := range []string{"t1", "t2", "t3"} {
		ok, err := bucket.Exists(ctx, FullMetaKey(tbl, 10))
		require.NoError(t, err)
		assert.True(t, ok, "meta for %s should exist", tbl)
	}
}

// TestExportIncremental_RequiresFullFirst verifies that ExportIncremental is a
// no-op when no full snapshot exists.
func TestExportIncremental_RequiresFullFirst(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{tableNames: []string{"orders"}, incrIdx: 50}

	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}

	ctx := context.Background()
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))

	// Bucket should remain empty.
	assert.Equal(t, 0, len(bucket.Objects()))
}

// TestExportIncremental_AfterFull verifies that ExportIncremental produces an
// incremental artefact once a full snapshot exists.
func TestExportIncremental_AfterFull(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"orders"},
		snapshotIdx:  100,
		incrIdx:      200,
		snapshotData: makeSnapshotData(t),
	}

	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}

	ctx := context.Background()
	// First, create a full
	require.NoError(t, exp.ExportFull(ctx, "orders"))
	// Now take an incremental.
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))

	incrSnapKey := IncrSnapKey("orders", 100, 200)
	incrMetaKey := IncrMetaKey("orders", 100, 200)

	ok, err := bucket.Exists(ctx, incrSnapKey)
	require.NoError(t, err)
	assert.True(t, ok, "incremental snap should exist")

	ok, err = bucket.Exists(ctx, incrMetaKey)
	require.NoError(t, err)
	assert.True(t, ok, "incremental meta should exist")

	r, err := bucket.Get(ctx, incrMetaKey)
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)

	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, SnapshotTypeIncremental, m.Type)
	assert.Equal(t, uint64(100), m.BaseIndex)
	assert.Equal(t, uint64(200), m.TipIndex)
}

// TestExportIncremental_ChainLimitSkips verifies that incremental export is a
// no-op once the chain length reaches IncrMaxChain.
func TestExportIncremental_ChainLimitSkips(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	maxChain := 3

	// We'll manually insert meta objects simulating a chain of length maxChain.
	ctx := context.Background()
	writeMeta := func(m Meta, key string) {
		data, _ := json.Marshal(m)
		require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(data)))
	}

	// Insert full meta at tip=100.
	writeMeta(Meta{
		Table: "t", Type: SnapshotTypeFull, TipIndex: 100, Format: SnapshotFormat,
	}, FullMetaKey("t", 100))

	// Insert incremental chain: 100→110, 110→120, 120→130.
	writeMeta(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 100, TipIndex: 110}, IncrMetaKey("t", 100, 110))
	writeMeta(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 110, TipIndex: 120}, IncrMetaKey("t", 110, 120))
	writeMeta(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 120, TipIndex: 130}, IncrMetaKey("t", 120, 130))

	svc := &fakeTableService{tableNames: []string{"t"}, snapshotIdx: 100, incrIdx: 140, snapshotData: makeSnapshotData(t)}
	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: maxChain,
	}, log: newTestLogger()}

	before := len(bucket.Objects())
	require.NoError(t, exp.ExportIncremental(ctx, "t"))
	after := len(bucket.Objects())

	assert.Equal(t, before, after, "ExportIncremental should be a no-op when chain is at max length")
}

// TestExportIncremental_NoNewData verifies ExportIncremental is a no-op when
// tip equals base (nothing changed).
func TestExportIncremental_NoNewData(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	// Pre-seed a full snapshot meta at index 50.
	data, _ := json.Marshal(Meta{
		Table: "orders", Type: SnapshotTypeFull, TipIndex: 50, Format: SnapshotFormat,
	})
	require.NoError(t, bucket.Upload(ctx, FullMetaKey("orders", 50), bytes.NewReader(data)))

	svc := &fakeTableService{
		tableNames:  []string{"orders"},
		snapshotIdx: 50,
		incrIdx:     50, // same as base → no new data
	}
	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 8,
	}, log: newTestLogger()}

	before := len(bucket.Objects())
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))
	after := len(bucket.Objects())

	assert.Equal(t, before, after, "no-op when tip == base")
}

// TestListMeta verifies ListMeta returns all committed artefacts in order.
func TestListMeta(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	uploadMeta := func(m Meta, key string) {
		data, _ := json.Marshal(m)
		require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(data)))
	}
	uploadMeta(Meta{Table: "t", Type: SnapshotTypeFull, TipIndex: 10}, FullMetaKey("t", 10))
	uploadMeta(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 10, TipIndex: 20}, IncrMetaKey("t", 10, 20))
	uploadMeta(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 20, TipIndex: 30}, IncrMetaKey("t", 20, 30))

	svc := &fakeTableService{tableNames: []string{"t"}}
	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n1",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 8,
	}, log: newTestLogger()}

	metas, err := exp.ListMeta(ctx, "t")
	require.NoError(t, err)
	require.Len(t, metas, 3)
	assert.Equal(t, uint64(10), metas[0].TipIndex)
	assert.Equal(t, uint64(20), metas[1].TipIndex)
	assert.Equal(t, uint64(30), metas[2].TipIndex)
}

// TestObjectKeyScheme validates all key helpers produce expected paths.
func TestObjectKeyScheme(t *testing.T) {
	assert.Equal(t, "snapshots/orders/full/100.snap", FullSnapKey("orders", 100))
	assert.Equal(t, "snapshots/orders/full/100.snap.meta", FullMetaKey("orders", 100))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap", IncrSnapKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap.meta", IncrMetaKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/.lease/node1", LeaseKey("orders", "node1"))
	assert.True(t, strings.HasPrefix(GCLogKey(time.Now()), "gc/"))
}

// Integration test: use a real filesystem bucket and simulate the full
// export + incremental chain + GC lifecycle.
func TestExporterGCIntegration(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"users"},
		snapshotIdx:  1000,
		incrIdx:      1100,
		snapshotData: makeSnapshotData(t),
	}
	cfg := ExporterConfig{
		Bucket:       bucket,
		NodeID:       "integration-node",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}
	exp := &SnapshotExporter{tables: svc, cfg: cfg, log: newTestLogger()}

	// --- Phase 1: Export full snapshot ---
	require.NoError(t, exp.ExportFull(ctx, "users"))
	metas, err := exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, SnapshotTypeFull, metas[0].Type)
	assert.Equal(t, uint64(1000), metas[0].TipIndex)

	// --- Phase 2: Export incremental ---
	require.NoError(t, exp.ExportIncremental(ctx, "users"))
	metas, err = exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	require.Len(t, metas, 2)
	assert.Equal(t, SnapshotTypeIncremental, metas[1].Type)
	assert.Equal(t, uint64(1000), metas[1].BaseIndex)
	assert.Equal(t, uint64(1100), metas[1].TipIndex)

	// --- Phase 3: GC with long retention (nothing deleted) ---
	gcCfg := GCConfig{
		Bucket:    bucket,
		Retention: 48 * time.Hour,
		Interval:  time.Hour,
	}
	gcWorker := NewGCWorker(gcCfg, newTestLogger())
	require.NoError(t, gcWorker.RunOnce(ctx))

	metas, err = exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	assert.Len(t, metas, 2, "GC should not delete recent artefacts")

	// --- Phase 4: Export a new full snapshot (simulating the next full cycle) ---
	svc.snapshotIdx = 2000
	require.NoError(t, exp.ExportFull(ctx, "users"))
	metas, err = exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	assert.Len(t, metas, 3, "two fulls + one incr")

	// --- Phase 5: GC with zero retention → old full + old incr deleted ---
	gcCfg.Retention = 0
	gcWorker = NewGCWorker(gcCfg, newTestLogger())
	require.NoError(t, gcWorker.RunOnce(ctx))

	metas, err = exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	// Only the newest full should survive (the incr at base=1000 is before the new
	// latest-full tip=2000 and should be deleted).
	assert.Len(t, metas, 1)
	assert.Equal(t, SnapshotTypeFull, metas[0].Type)
	assert.Equal(t, uint64(2000), metas[0].TipIndex)
}

// TestExporter_SHA256IsConsistent verifies that successive calls with the same
// data produce the same SHA256 in the meta.
func TestExporter_SHA256IsConsistent(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tableNames:   []string{"t"},
		snapshotIdx:  1,
		snapshotData: makeSnapshotData(t),
	}
	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}
	require.NoError(t, exp.ExportFull(ctx, "t"))

	r, err := bucket.Get(ctx, FullMetaKey("t", 1))
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)
	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))

	assert.Len(t, m.SHA256, 64, "SHA256 should be 64 hex characters")
}

// TestSnapshotFileFormat_WrittenToReader verifies that an ExportFull artefact
// can be round-tripped through the replication/snapshot reader.
func TestSnapshotFileFormat_WrittenToReader(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()

	// Write a real command via the snapshotFile format.
	cmd := &armadapb.Command{
		Table: []byte("test"),
		Type:  armadapb.Command_PUT,
		Kv:    &armadapb.KeyValue{Key: []byte("hello"), Value: []byte("world")},
	}
	cmdBytes, err := cmd.MarshalVT()
	require.NoError(t, err)

	svc := &fakeTableService{
		tableNames:   []string{"test"},
		snapshotIdx:  7,
		snapshotData: cmdBytes,
	}
	exp := &SnapshotExporter{tables: svc, cfg: ExporterConfig{
		Bucket:       bucket,
		NodeID:       "n",
		FullInterval: time.Hour,
		IncrInterval: 30 * time.Minute,
		IncrMaxChain: 4,
	}, log: newTestLogger()}
	require.NoError(t, exp.ExportFull(ctx, "test"))

	// Download the snap and verify we can read back the commands.
	r, err := bucket.Get(ctx, FullSnapKey("test", 7))
	require.NoError(t, err)
	defer r.Close()

	snapData, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NotEmpty(t, snapData, "snap should not be empty")

	// Parse via the snapshotFile reader (snappy+length-prefix).
	sf, err := snapshot.NewTemp()
	require.NoError(t, err)
	defer sf.Close()
	_, err = sf.File.Write(snapData)
	require.NoError(t, err)
	_, err = sf.File.Seek(0, 0)
	require.NoError(t, err)

	// Read back commands (should include the PUT command and the trailing DUMMY).
	var readCmds []*armadapb.Command
	buf := make([]byte, 64*1024)
	for {
		n, readErr := sf.Read(buf)
		if readErr != nil {
			break
		}
		if n == 0 {
			continue
		}
		var c armadapb.Command
		if unmarshalErr := c.UnmarshalVT(buf[:n]); unmarshalErr != nil {
			break
		}
		readCmds = append(readCmds, &c)
	}

	require.GreaterOrEqual(t, len(readCmds), 1, "should read at least the DUMMY command")
	// The last command must be a DUMMY with the leader index set.
	last := readCmds[len(readCmds)-1]
	assert.Equal(t, armadapb.Command_DUMMY, last.Type)
	require.NotNil(t, last.LeaderIndex)
	assert.Equal(t, uint64(7), *last.LeaderIndex)
}
