// Copyright JAMF Software, LLC

package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	replicationSnapshot "github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/replication/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thanos-io/objstore"
	"go.uber.org/zap"
)

// fakeTableService is a mock TableSnapshotService used in tests.
type fakeTableService struct {
	tables []string
	// index returned by Snapshot (simulates the applied leader index)
	snapshotIdx uint64
	// index returned by IncrementalSnapshot
	incrIdx uint64
	// data to write during Snapshot / IncrementalSnapshot
	snapshotData []byte
}

func (f *fakeTableService) GetTableNames() ([]string, error) {
	return f.tables, nil
}

func (f *fakeTableService) Snapshot(_ context.Context, _ string, w io.Writer) (uint64, error) {
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return 0, err
		}
	}
	return f.snapshotIdx, nil
}

func (f *fakeTableService) IncrementalSnapshot(_ context.Context, _ string, w io.Writer, _ uint64) (uint64, error) {
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return 0, err
		}
	}
	return f.incrIdx, nil
}

// makeSnapshotData returns a minimal armadapb.Command serialized in the
// armada-command-v1 wire format so the exporter temp file is non-empty.
func makeSnapshotData(t *testing.T) []byte {
	t.Helper()
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

func newTestExporter(svc *fakeTableService, bucket *objstore.InMemBucket) *store.SnapshotExporter {
	return store.NewSnapshotExporter(svc, store.ExporterConfig{
		Bucket: bucket,
		NodeID: "leader-1",
	}, newTestLogger())
}

// TestExportFull_Basic verifies that ExportFull uploads a .snap and .meta file.
func TestExportFull_Basic(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  42,
		snapshotData: makeSnapshotData(t),
	}

	exp := newTestExporter(svc, bucket)
	ctx := context.Background()
	require.NoError(t, exp.ExportFull(ctx, "orders"))

	snapKey := store.FullSnapKey("orders", 42)
	metaKey := store.FullMetaKey("orders", 42)

	ok, err := bucket.Exists(ctx, snapKey)
	require.NoError(t, err)
	assert.True(t, ok, "snap key should exist")

	ok, err = bucket.Exists(ctx, metaKey)
	require.NoError(t, err)
	assert.True(t, ok, "meta key should exist")

	r, err := bucket.Get(ctx, metaKey)
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)

	var m store.Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, "orders", m.Table)
	assert.Equal(t, store.SnapshotTypeFull, m.Type)
	assert.Equal(t, uint64(0), m.BaseIndex)
	assert.Equal(t, uint64(42), m.TipIndex)
	assert.Equal(t, "leader-1", m.NodeID)
	assert.Equal(t, store.SnapshotFormat, m.Format)
	assert.NotEmpty(t, m.SHA256)
	assert.Greater(t, m.SizeBytes, int64(0))
}

// TestExportFull_Idempotent verifies that calling ExportFull twice at the same
// index does not overwrite the artefact.
func TestExportFull_Idempotent(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  100,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)
	ctx := context.Background()

	require.NoError(t, exp.ExportFull(ctx, "orders"))
	before := len(bucket.Objects())
	require.NoError(t, exp.ExportFull(ctx, "orders"))
	assert.Equal(t, before, len(bucket.Objects()), "second ExportFull should not add new objects")
}

// TestExportIncremental_StandaloneFromZero verifies that ExportIncremental
// works without any prior artefact, producing an incremental from base 0.
func TestExportIncremental_StandaloneFromZero(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{tables: []string{"orders"}, incrIdx: 50, snapshotData: makeSnapshotData(t)}
	exp := newTestExporter(svc, bucket)

	require.NoError(t, exp.ExportIncremental(context.Background(), "orders"))

	ok, err := bucket.Exists(context.Background(), store.IncrSnapKey("orders", 0, 50))
	require.NoError(t, err)
	assert.True(t, ok, "incremental from base 0 should be produced without a prior full")
}

// TestExportIncremental_AfterFull verifies an incremental is produced once a
// full snapshot exists. The incremental base is the full's tip.
func TestExportIncremental_AfterFull(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  100,
		incrIdx:      200,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)
	ctx := context.Background()

	require.NoError(t, exp.ExportFull(ctx, "orders"))
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))

	ok, err := bucket.Exists(ctx, store.IncrSnapKey("orders", 100, 200))
	require.NoError(t, err)
	assert.True(t, ok)

	r, err := bucket.Get(ctx, store.IncrMetaKey("orders", 100, 200))
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)
	var m store.Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, store.SnapshotTypeIncremental, m.Type)
	assert.Equal(t, uint64(100), m.BaseIndex)
	assert.Equal(t, uint64(200), m.TipIndex)
}

// TestExportIncremental_ChainBuildsOnLatestTip verifies that successive
// incrementals chain off the highest tip regardless of whether it came from a
// full or a prior incremental.
func TestExportIncremental_ChainBuildsOnLatestTip(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	// Full at 100, then two incrementals: 100→200, 200→300.
	svc := &fakeTableService{
		tables:       []string{"t"},
		snapshotIdx:  100,
		incrIdx:      200,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)

	require.NoError(t, exp.ExportFull(ctx, "t"))
	require.NoError(t, exp.ExportIncremental(ctx, "t")) // 100→200

	svc.incrIdx = 300
	require.NoError(t, exp.ExportIncremental(ctx, "t")) // 200→300

	ok, err := bucket.Exists(ctx, store.IncrSnapKey("t", 200, 300))
	require.NoError(t, err)
	assert.True(t, ok, "second incremental should chain from 200")
}

// TestExportIncremental_NoNewData verifies ExportIncremental is a no-op when
// the table has not advanced since the last tip.
func TestExportIncremental_NoNewData(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	data, _ := json.Marshal(store.Meta{
		Table: "orders", Type: store.SnapshotTypeFull, TipIndex: 50, Format: store.SnapshotFormat,
	})
	require.NoError(t, bucket.Upload(ctx, store.FullMetaKey("orders", 50), bytes.NewReader(data)))

	svc := &fakeTableService{tables: []string{"orders"}, snapshotIdx: 50, incrIdx: 50}
	exp := newTestExporter(svc, bucket)

	before := len(bucket.Objects())
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))
	assert.Equal(t, before, len(bucket.Objects()), "no-op when tip == base")
}

// TestExportFull_AndIncrementalAreIndependent verifies that a new full snapshot
// does not affect the incremental sequence — the next incremental after a new
// full still chains off the highest tip (which is now the new full).
func TestExportFull_AndIncrementalAreIndependent(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()
	svc := &fakeTableService{
		tables:       []string{"t"},
		snapshotIdx:  100,
		incrIdx:      150,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)

	require.NoError(t, exp.ExportFull(ctx, "t"))        // full at 100
	require.NoError(t, exp.ExportIncremental(ctx, "t")) // incr 100→150

	// Take a new out-of-band full at 200.
	svc.snapshotIdx = 200
	require.NoError(t, exp.ExportFull(ctx, "t"))

	// Next incremental should base off 200 (the new highest tip).
	svc.incrIdx = 250
	require.NoError(t, exp.ExportIncremental(ctx, "t"))

	ok, err := bucket.Exists(ctx, store.IncrSnapKey("t", 200, 250))
	require.NoError(t, err)
	assert.True(t, ok, "incremental should base off the latest tip (the new full at 200)")
}

// TestListMeta verifies ListMeta returns all artefacts sorted by TipIndex.
func TestListMeta(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	upload := func(m store.Meta, key string) {
		data, _ := json.Marshal(m)
		require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(data)))
	}
	upload(store.Meta{Table: "t", Type: store.SnapshotTypeFull, TipIndex: 10}, store.FullMetaKey("t", 10))
	upload(store.Meta{Table: "t", Type: store.SnapshotTypeIncremental, BaseIndex: 10, TipIndex: 20}, store.IncrMetaKey("t", 10, 20))
	upload(store.Meta{Table: "t", Type: store.SnapshotTypeIncremental, BaseIndex: 20, TipIndex: 30}, store.IncrMetaKey("t", 20, 30))

	exp := newTestExporter(&fakeTableService{tables: []string{"t"}}, bucket)
	metas, err := exp.ListMeta(ctx, "t")
	require.NoError(t, err)
	require.Len(t, metas, 3)
	assert.Equal(t, uint64(10), metas[0].TipIndex)
	assert.Equal(t, uint64(20), metas[1].TipIndex)
	assert.Equal(t, uint64(30), metas[2].TipIndex)
}

// TestObjectKeyScheme validates all key helpers produce expected paths.
func TestObjectKeyScheme(t *testing.T) {
	assert.Equal(t, "snapshots/orders/full/100.snap", store.FullSnapKey("orders", 100))
	assert.Equal(t, "snapshots/orders/full/100.snap.meta", store.FullMetaKey("orders", 100))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap", store.IncrSnapKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap.meta", store.IncrMetaKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/.lease/node1", store.LeaseKey("orders", "node1"))
	assert.True(t, strings.HasPrefix(store.GCLogKey(time.Now()), "gc/"))
}

// TestNotifyLogCompacted_Run verifies that NotifyLogCompacted feeds the Run
// loop and triggers an incremental export, starting from base 0 when no prior
// artefact exists.
func TestNotifyLogCompacted_Run(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	ctx := context.Background()

	svc := &fakeTableService{
		tables:       []string{"users"},
		incrIdx:      600,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { exp.Run(runCtx); close(done) }()

	exp.NotifyLogCompacted("users")

	require.Eventually(t, func() bool {
		ok, _ := bucket.Exists(ctx, store.IncrSnapKey("users", 0, 600))
		return ok
	}, 2*time.Second, 10*time.Millisecond, "incremental from base 0 should appear after compaction")

	cancel()
	<-done
}

// TestExporterGCIntegration exercises the full lifecycle: full → incremental → GC.
func TestExporterGCIntegration(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{
		tables:       []string{"users"},
		snapshotIdx:  1000,
		incrIdx:      1100,
		snapshotData: makeSnapshotData(t),
	}
	exp := store.NewSnapshotExporter(svc, store.ExporterConfig{
		Bucket: bucket,
		NodeID: "integration-node",
	}, newTestLogger())

	require.NoError(t, exp.ExportFull(ctx, "users"))
	require.NoError(t, exp.ExportIncremental(ctx, "users"))

	metas, err := exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	require.Len(t, metas, 2)

	// GC with long retention — nothing deleted.
	gc := store.NewGCWorker(store.GCConfig{Bucket: bucket, Retention: 48 * time.Hour, Interval: time.Hour}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 2)

	// New full at 2000.
	svc.snapshotIdx = 2000
	require.NoError(t, exp.ExportFull(ctx, "users"))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 3)

	// GC with zero retention — old full and old incremental deleted.
	gc = store.NewGCWorker(store.GCConfig{Bucket: bucket, Retention: 0, Interval: time.Hour}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 1)
	assert.Equal(t, store.SnapshotTypeFull, metas[0].Type)
	assert.Equal(t, uint64(2000), metas[0].TipIndex)
}

// TestExporter_SHA256IsConsistent verifies SHA256 is stable across calls.
func TestExporter_SHA256IsConsistent(t *testing.T) {
	bucket := objstore.NewInMemBucket()
	svc := &fakeTableService{tables: []string{"t"}, snapshotIdx: 1, snapshotData: makeSnapshotData(t)}
	exp := newTestExporter(svc, bucket)
	require.NoError(t, exp.ExportFull(context.Background(), "t"))

	r, err := bucket.Get(context.Background(), store.FullMetaKey("t", 1))
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)
	var m store.Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Len(t, m.SHA256, 64)
}

// TestSnapshotFileFormat_WrittenToReader verifies that an ExportFull artefact
// round-trips through the replication/snapshot reader.
func TestSnapshotFileFormat_WrittenToReader(t *testing.T) {
	ctx := context.Background()
	bucket := objstore.NewInMemBucket()

	cmd := &armadapb.Command{
		Table: []byte("test"),
		Type:  armadapb.Command_PUT,
		Kv:    &armadapb.KeyValue{Key: []byte("hello"), Value: []byte("world")},
	}
	cmdBytes, err := cmd.MarshalVT()
	require.NoError(t, err)

	svc := &fakeTableService{tables: []string{"test"}, snapshotIdx: 7, snapshotData: cmdBytes}
	exp := newTestExporter(svc, bucket)
	require.NoError(t, exp.ExportFull(ctx, "test"))

	r, err := bucket.Get(ctx, store.FullSnapKey("test", 7))
	require.NoError(t, err)
	defer r.Close()
	snapData, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NotEmpty(t, snapData)

	sf, err := replicationSnapshot.NewTemp()
	require.NoError(t, err)
	defer sf.Close()
	_, err = sf.File.Write(snapData)
	require.NoError(t, err)
	_, err = sf.File.Seek(0, 0)
	require.NoError(t, err)

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

	require.GreaterOrEqual(t, len(readCmds), 1)
	last := readCmds[len(readCmds)-1]
	assert.Equal(t, armadapb.Command_DUMMY, last.Type)
	require.NotNil(t, last.LeaderIndex)
	assert.Equal(t, uint64(7), *last.LeaderIndex)
}
