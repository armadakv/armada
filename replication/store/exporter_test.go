// Copyright JAMF Software, LLC

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/armadakv/armada/armadapb"
	replicationSnapshot "github.com/armadakv/armada/replication/snapshot"
	"github.com/armadakv/armada/storage/table/fsm"
	"github.com/armadakv/objfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	// gcHorizon returned by GCHorizon and the consistent incremental response.
	gcHorizon    uint64
	gcHorizonErr error
	incrSnapshot func(context.Context, string, io.Writer, uint64) (*fsm.SnapshotResponse, error)
	// leader reported by IsLeader
	leader bool
	// fullExports counts ExportFull fall-throughs observed via Snapshot
	fullExports int
}

func (f *fakeTableService) GetTableNames() ([]string, error) {
	return f.tables, nil
}

func (f *fakeTableService) GCHorizon(_ context.Context, _ string) (uint64, error) {
	return f.gcHorizon, f.gcHorizonErr
}

func (f *fakeTableService) IsLeader(_ string) (bool, error) {
	return f.leader, nil
}

func (f *fakeTableService) Snapshot(_ context.Context, _ string, w io.Writer) (uint64, error) {
	f.fullExports++
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return 0, err
		}
	}
	return f.snapshotIdx, nil
}

func (f *fakeTableService) IncrementalSnapshot(ctx context.Context, tableName string, w io.Writer, sinceIndex uint64) (*fsm.SnapshotResponse, error) {
	if f.incrSnapshot != nil {
		return f.incrSnapshot(ctx, tableName, w, sinceIndex)
	}
	if len(f.snapshotData) > 0 {
		if _, err := w.Write(f.snapshotData); err != nil {
			return nil, err
		}
	}
	baseIndex := sinceIndex
	if f.gcHorizon > 0 && baseIndex <= f.gcHorizon {
		baseIndex = f.gcHorizon + 1
	}
	return &fsm.SnapshotResponse{
		Index:     f.incrIdx,
		TipIndex:  f.incrIdx,
		BaseIndex: baseIndex,
		GCHorizon: f.gcHorizon,
	}, nil
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

func newTestExporter(svc *fakeTableService, bucket objfs.Bucket) *SnapshotExporter {
	return NewSnapshotExporter(svc, ExporterConfig{
		Bucket: bucket,
		NodeID: "leader-1",
	}, newTestLogger())
}

// TestExportFull_Basic verifies that ExportFull uploads a .snap and .meta file.
func TestExportFull_Basic(t *testing.T) {
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  42,
		snapshotData: makeSnapshotData(t),
	}

	exp := newTestExporter(svc, bucket)
	ctx := context.Background()
	require.NoError(t, exp.ExportFull(ctx, "orders"))

	snapKey := FullSnapKey("orders", 42)
	metaKey := FullMetaKey("orders", 42)

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

	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, "orders", m.Table)
	assert.Equal(t, SnapshotTypeFull, m.Type)
	assert.Equal(t, uint64(0), m.BaseIndex)
	assert.Equal(t, uint64(42), m.TipIndex)
	assert.Equal(t, "leader-1", m.NodeID)
	assert.Equal(t, SnapshotFormat, m.Format)
	assert.NotEmpty(t, m.SHA256)
	assert.Positive(t, m.SizeBytes, int64(0))
}

// TestExportFull_Idempotent verifies that calling ExportFull twice at the same
// index does not overwrite the artefact.
func TestExportFull_Idempotent(t *testing.T) {
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  100,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)
	ctx := context.Background()

	require.NoError(t, exp.ExportFull(ctx, "orders"))
	before := ObjectCount(t, bucket, "")
	require.NoError(t, exp.ExportFull(ctx, "orders"))
	assert.Equal(t, before, ObjectCount(t, bucket, ""), "second ExportFull should not add new objects")
}

// TestExportIncremental_StandaloneFromZero verifies that ExportIncremental
// works without any prior artefact, producing an incremental from base 0.
func TestExportIncremental_StandaloneFromZero(t *testing.T) {
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{tables: []string{"orders"}, incrIdx: 50, snapshotData: makeSnapshotData(t)}
	exp := newTestExporter(svc, bucket)

	require.NoError(t, exp.ExportIncremental(context.Background(), "orders"))

	ok, err := bucket.Exists(context.Background(), IncrSnapKey("orders", 0, 50))
	require.NoError(t, err)
	assert.True(t, ok, "incremental from base 0 should be produced without a prior full")
}

// TestExportIncremental_AfterFull verifies an incremental is produced once a
// full snapshot exists. The incremental base is the full's tip.
func TestExportIncremental_AfterFull(t *testing.T) {
	bucket := NewLocalBucket(t)
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

	ok, err := bucket.Exists(ctx, IncrSnapKey("orders", 100, 200))
	require.NoError(t, err)
	assert.True(t, ok)

	r, err := bucket.Get(ctx, IncrMetaKey("orders", 100, 200))
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)
	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Equal(t, SnapshotTypeIncremental, m.Type)
	assert.Equal(t, uint64(100), m.BaseIndex)
	assert.Equal(t, uint64(200), m.TipIndex)
}

// TestExportIncremental_ChainBuildsOnLatestTip verifies that successive
// incrementals chain off the highest tip regardless of whether it came from a
// full or a prior incremental.
func TestExportIncremental_ChainBuildsOnLatestTip(t *testing.T) {
	bucket := NewLocalBucket(t)
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

	ok, err := bucket.Exists(ctx, IncrSnapKey("t", 200, 300))
	require.NoError(t, err)
	assert.True(t, ok, "second incremental should chain from 200")
}

// TestExportIncremental_NoNewData verifies ExportIncremental is a no-op when
// the table has not advanced since the last tip.
func TestExportIncremental_NoNewData(t *testing.T) {
	bucket := NewLocalBucket(t)
	ctx := context.Background()

	data, _ := json.Marshal(Meta{
		Table: "orders", Type: SnapshotTypeFull, TipIndex: 50, Format: SnapshotFormat,
	})
	require.NoError(t, bucket.Upload(ctx, FullMetaKey("orders", 50), bytes.NewReader(data)))

	svc := &fakeTableService{tables: []string{"orders"}, snapshotIdx: 50, incrIdx: 50}
	exp := newTestExporter(svc, bucket)

	before := ObjectCount(t, bucket, "")
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))
	assert.Equal(t, before, ObjectCount(t, bucket, ""), "no-op when tip == base")
}

// TestExportFull_AndIncrementalAreIndependent verifies that a new full snapshot
// does not affect the incremental sequence — the next incremental after a new
// full still chains off the highest tip (which is now the new full).
func TestExportFull_AndIncrementalAreIndependent(t *testing.T) {
	bucket := NewLocalBucket(t)
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

	ok, err := bucket.Exists(ctx, IncrSnapKey("t", 200, 250))
	require.NoError(t, err)
	assert.True(t, ok, "incremental should base off the latest tip (the new full at 200)")
}

// TestListMeta verifies ListMeta returns all artefacts sorted by TipIndex.
func TestListMeta(t *testing.T) {
	bucket := NewLocalBucket(t)
	ctx := context.Background()

	upload := func(m Meta, key string) {
		data, _ := json.Marshal(m)
		require.NoError(t, bucket.Upload(ctx, key, bytes.NewReader(data)))
	}
	upload(Meta{Table: "t", Type: SnapshotTypeFull, TipIndex: 10}, FullMetaKey("t", 10))
	upload(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 10, TipIndex: 20}, IncrMetaKey("t", 10, 20))
	upload(Meta{Table: "t", Type: SnapshotTypeIncremental, BaseIndex: 20, TipIndex: 30}, IncrMetaKey("t", 20, 30))

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
	assert.Equal(t, "snapshots/orders/full/100.snap", FullSnapKey("orders", 100))
	assert.Equal(t, "snapshots/orders/full/100.snap.meta", FullMetaKey("orders", 100))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap", IncrSnapKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/incr/100_200.snap.meta", IncrMetaKey("orders", 100, 200))
	assert.Equal(t, "snapshots/orders/.lease/node1", LeaseKey("orders", "node1"))
	assert.True(t, strings.HasPrefix(GCLogKey(time.Now()), "gc/"))
}

// TestNotifyLogCompacted_Run verifies that NotifyLogCompacted feeds the Run
// loop and triggers an incremental export, starting from base 0 when no prior
// artefact exists.
func TestNotifyLogCompacted_Run(t *testing.T) {
	bucket := NewLocalBucket(t)
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
		ok, _ := bucket.Exists(ctx, IncrSnapKey("users", 0, 600))
		return ok
	}, 2*time.Second, 10*time.Millisecond, "incremental from base 0 should appear after compaction")

	cancel()
	<-done
}

// TestExporterGCIntegration exercises the full lifecycle: full → incremental → GC.
func TestExporterGCIntegration(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"users"},
		snapshotIdx:  1000,
		incrIdx:      1100,
		snapshotData: makeSnapshotData(t),
	}
	exp := NewSnapshotExporter(svc, ExporterConfig{
		Bucket: bucket,
		NodeID: "integration-node",
	}, newTestLogger())

	require.NoError(t, exp.ExportFull(ctx, "users"))
	require.NoError(t, exp.ExportIncremental(ctx, "users"))

	metas, err := exp.ListMeta(ctx, "users")
	require.NoError(t, err)
	require.Len(t, metas, 2)

	// GC with long retention — nothing deleted.
	gc := NewGCWorker(GCConfig{Bucket: bucket, Retention: 48 * time.Hour, Interval: time.Hour}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 2)

	// New full at 2000.
	svc.snapshotIdx = 2000
	require.NoError(t, exp.ExportFull(ctx, "users"))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 3)

	// GC with zero retention — old full and old incremental deleted.
	gc = NewGCWorker(GCConfig{Bucket: bucket, Retention: 0, Interval: time.Hour}, newTestLogger())
	require.NoError(t, gc.RunOnce(ctx))
	metas, _ = exp.ListMeta(ctx, "users")
	assert.Len(t, metas, 1)
	assert.Equal(t, SnapshotTypeFull, metas[0].Type)
	assert.Equal(t, uint64(2000), metas[0].TipIndex)
}

// TestExporter_SHA256IsConsistent verifies SHA256 is stable across calls.
func TestExporter_SHA256IsConsistent(t *testing.T) {
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{tables: []string{"t"}, snapshotIdx: 1, snapshotData: makeSnapshotData(t)}
	exp := newTestExporter(svc, bucket)
	require.NoError(t, exp.ExportFull(context.Background(), "t"))

	r, err := bucket.Get(context.Background(), FullMetaKey("t", 1))
	require.NoError(t, err)
	defer r.Close()
	data, _ := io.ReadAll(r)
	var m Meta
	require.NoError(t, json.Unmarshal(data, &m))
	assert.Len(t, m.SHA256, 64)
}

// TestSnapshotFileFormat_WrittenToReader verifies that an ExportFull artefact
// round-trips through the replication/snapshot reader.
func TestSnapshotFileFormat_WrittenToReader(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)

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

	r, err := bucket.Get(ctx, FullSnapKey("test", 7))
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
	_, err = sf.Seek(0, 0)
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

// TestExportIncremental_GCHorizonGuard verifies that the exporter never
// publishes a delta whose base is at or below the GC horizon — such a delta is
// silently short, because the compacted MVCC versions it needs, tombstones
// included, are gone — and that it achieves this by re-basing at the horizon
// rather than by giving up and writing a full.
//
// The distinction is the whole feature. Exports are driven by log compaction,
// so the previous artefact's tip sits about snapshot-entries behind the current
// index while the horizon is only about compaction-overhead behind it. Under
// any configuration where snapshot-entries exceeds compaction-overhead — such
// as the 1000/200 defaults — chaining onto that tip puts the base below the
// horizon on every single export, so falling back to a full made the
// incremental path unreachable rather than merely rare.
func TestExportIncremental_GCHorizonGuard(t *testing.T) {
	ctx := context.Background()

	t.Run("base above the horizon exports an incremental", func(t *testing.T) {
		bucket := NewLocalBucket(t)
		svc := &fakeTableService{
			tables:       []string{"orders"},
			snapshotIdx:  200,
			incrIdx:      150,
			gcHorizon:    50,
			snapshotData: makeSnapshotData(t),
		}
		exp := newTestExporter(svc, bucket)
		require.NoError(t, exp.ExportFull(ctx, "orders")) // base at 200
		svc.incrIdx = 260
		svc.snapshotIdx = 260
		require.NoError(t, exp.ExportIncremental(ctx, "orders"))

		ok, err := bucket.Exists(ctx, IncrMetaKey("orders", 200, 260))
		require.NoError(t, err)
		assert.True(t, ok, "incremental should have been published")
	})

	t.Run("a base at or below the horizon is re-based just above it", func(t *testing.T) {
		bucket := NewLocalBucket(t)
		svc := &fakeTableService{
			tables:       []string{"orders"},
			snapshotIdx:  100,
			incrIdx:      180,
			snapshotData: makeSnapshotData(t),
		}
		exp := newTestExporter(svc, bucket)
		require.NoError(t, exp.ExportFull(ctx, "orders")) // tip 100

		// GC has now advanced past the only available base.
		svc.gcHorizon = 120
		svc.snapshotIdx = 180
		require.NoError(t, exp.ExportIncremental(ctx, "orders"))

		ok, err := bucket.Exists(ctx, IncrMetaKey("orders", 100, 180))
		require.NoError(t, err)
		assert.False(t, ok, "the unsound base must not be published")
		ok, err = bucket.Exists(ctx, FullMetaKey("orders", 180))
		require.NoError(t, err)
		assert.False(t, ok, "re-basing should make a full unnecessary")

		// 121, not 120: the follower-side check rejects an artefact whose base
		// is at or below its own recorded horizon, so the base has to clear it
		// strictly.
		ok, err = bucket.Exists(ctx, IncrMetaKey("orders", 121, 180))
		require.NoError(t, err)
		assert.True(t, ok, "the delta should have been re-based at gcHorizon+1")

		metas, err := ListMeta(ctx, bucket, "orders")
		require.NoError(t, err)
		for _, m := range metas {
			if m.Type == SnapshotTypeIncremental {
				assert.Greater(t, m.BaseIndex, m.GCHorizon,
					"a published delta must always be sound for a follower at its base")
			}
		}
	})

	t.Run("no artefact is published when the horizon has caught up to the tip", func(t *testing.T) {
		// Re-basing can put the base at or past the applied index. An artefact
		// with base >= tip can never be selected (the rule is
		// base <= followerIndex < tip), so it must not be written at all.
		bucket := NewLocalBucket(t)
		svc := &fakeTableService{
			tables:       []string{"orders"},
			snapshotIdx:  100,
			incrIdx:      100,
			snapshotData: makeSnapshotData(t),
		}
		exp := newTestExporter(svc, bucket)
		require.NoError(t, exp.ExportFull(ctx, "orders")) // tip 100

		svc.gcHorizon = 100
		require.NoError(t, exp.ExportIncremental(ctx, "orders"))

		metas, err := ListMeta(ctx, bucket, "orders")
		require.NoError(t, err)
		for _, m := range metas {
			assert.NotEqual(t, SnapshotTypeIncremental, m.Type,
				"nothing useful can be published here, so nothing should be")
		}
	})
}

// TestExportIncremental_ChainCap verifies that the incremental chain hanging off
// the newest full snapshot is bounded.
func TestExportIncremental_ChainCap(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  100,
		snapshotData: makeSnapshotData(t),
	}
	exp := NewSnapshotExporter(svc, ExporterConfig{
		Bucket:       bucket,
		NodeID:       "leader-1",
		IncrMaxChain: 2,
	}, newTestLogger())

	require.NoError(t, exp.ExportFull(ctx, "orders"))
	for i, tip := range []uint64{110, 120} {
		svc.incrIdx = tip
		require.NoError(t, exp.ExportIncremental(ctx, "orders"))
		ok, err := bucket.Exists(ctx, IncrMetaKey("orders", []uint64{100, 110}[i], tip))
		require.NoError(t, err)
		assert.True(t, ok)
	}

	// The third call hits the cap and writes a full snapshot instead.
	svc.incrIdx = 130
	svc.snapshotIdx = 130
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))
	ok, err := bucket.Exists(ctx, IncrMetaKey("orders", 120, 130))
	require.NoError(t, err)
	assert.False(t, ok, "the chain cap should have forced a full snapshot")
	ok, err = bucket.Exists(ctx, FullMetaKey("orders", 130))
	require.NoError(t, err)
	assert.True(t, ok)
}

// TestExportMetaRecordsGCHorizon checks that the horizon travels with the
// artefact, which is what lets a follower reading the bucket directly decide
// offline whether a delta is safe for it.
func TestExportMetaRecordsGCHorizon(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  90,
		gcHorizon:    17,
		snapshotData: makeSnapshotData(t),
	}
	exp := newTestExporter(svc, bucket)
	require.NoError(t, exp.ExportFull(ctx, "orders"))

	metas, err := exp.ListMeta(ctx, "orders")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	assert.Equal(t, uint64(17), metas[0].GCHorizon)
}

// TestExportIncremental_UsesMetadataAndRecordsFromSnapshotView covers the
// interleaving where the previously exported tip becomes stale while the FSM
// opens its snapshot view. The exporter must publish the rebased metadata and
// exactly the records returned by that same view, never the old horizon with
// records that require the new base.
func TestExportIncremental_UsesMetadataAndRecordsFromSnapshotView(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	leaderIndex := uint64(200)
	record := &armadapb.Command{
		Table:       []byte("orders"),
		Type:        armadapb.Command_PUT,
		LeaderIndex: &leaderIndex,
		Kv:          &armadapb.KeyValue{Key: []byte("after-gc"), Value: []byte("value")},
	}
	recordData, err := record.MarshalVT()
	require.NoError(t, err)

	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  100,
		snapshotData: makeSnapshotData(t),
		incrSnapshot: func(_ context.Context, _ string, w io.Writer, requestedBase uint64) (*fsm.SnapshotResponse, error) {
			require.Equal(t, uint64(100), requestedBase)
			_, err := w.Write(recordData)
			require.NoError(t, err)
			// GC advanced after chain selection but before the FSM snapshot view
			// opened. The view rebases to 151 and emits only its post-GC record.
			return &fsm.SnapshotResponse{TipIndex: 200, BaseIndex: 151, GCHorizon: 150}, nil
		},
	}
	exp := newTestExporter(svc, bucket)
	require.NoError(t, exp.ExportFull(ctx, "orders"))
	require.NoError(t, exp.ExportIncremental(ctx, "orders"))

	metas, err := exp.ListMeta(ctx, "orders")
	require.NoError(t, err)
	require.Len(t, metas, 2)
	meta := metas[1]
	assert.Equal(t, uint64(151), meta.BaseIndex)
	assert.Equal(t, uint64(200), meta.TipIndex)
	assert.Equal(t, uint64(150), meta.GCHorizon)
	assert.Greater(t, meta.BaseIndex, meta.GCHorizon)

	r, err := bucket.Get(ctx, IncrSnapKey("orders", meta.BaseIndex, meta.TipIndex))
	require.NoError(t, err)
	defer r.Close()
	snapData, err := io.ReadAll(r)
	require.NoError(t, err)

	sf, err := replicationSnapshot.NewTemp()
	require.NoError(t, err)
	defer sf.Close()
	_, err = sf.File.Write(snapData)
	require.NoError(t, err)
	_, err = sf.Seek(0, io.SeekStart)
	require.NoError(t, err)

	buf := make([]byte, 64*1024)
	n, err := sf.Read(buf)
	require.NoError(t, err)
	var exported armadapb.Command
	require.NoError(t, exported.UnmarshalVT(buf[:n]))
	assert.Equal(t, armadapb.Command_PUT, exported.Type)
	assert.Equal(t, []byte("after-gc"), exported.Kv.Key)
	require.NotNil(t, exported.LeaderIndex)
	assert.Equal(t, leaderIndex, *exported.LeaderIndex)
	assert.Greater(t, *exported.LeaderIndex, meta.BaseIndex,
		"a rebased artifact must not contain records from at or below its base")
	assert.LessOrEqual(t, *exported.LeaderIndex, meta.TipIndex)
}

type uploadFailingBucket struct {
	objfs.Bucket
	failMeta bool
}

func (b uploadFailingBucket) Upload(ctx context.Context, name string, r io.Reader, opts ...objfs.UploadOption) error {
	if b.failMeta && strings.HasSuffix(name, ".meta") {
		return errors.New("metadata upload failed")
	}
	return b.Bucket.Upload(ctx, name, r, opts...)
}

func TestExportFull_GCHorizonFailureDoesNotLeaveArtifact(t *testing.T) {
	ctx := context.Background()
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		snapshotIdx:  42,
		snapshotData: makeSnapshotData(t),
		gcHorizonErr: errors.New("gc horizon unavailable"),
	}

	err := newTestExporter(svc, bucket).ExportFull(ctx, "orders")
	require.ErrorContains(t, err, "read gc horizon")
	exists, err := bucket.Exists(ctx, FullSnapKey("orders", 42))
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestPublishArtifact_CleansUpAfterMetadataUploadFailure(t *testing.T) {
	ctx := context.Background()
	base := NewLocalBucket(t)
	bucket := uploadFailingBucket{Bucket: base, failMeta: true}
	svc := &fakeTableService{snapshotIdx: 42, snapshotData: makeSnapshotData(t)}

	err := newTestExporter(svc, bucket).ExportFull(ctx, "orders")
	require.ErrorContains(t, err, "upload meta")
	exists, existsErr := base.Exists(ctx, FullSnapKey("orders", 42))
	require.NoError(t, existsErr)
	assert.False(t, exists, "uncommitted artifact must be deleted")
}

func TestExporterRunExportsFullOnlyOnTheLeader(t *testing.T) {
	bucket := NewLocalBucket(t)
	svc := &fakeTableService{
		tables:       []string{"orders"},
		snapshotIdx:  7,
		snapshotData: makeSnapshotData(t),
	}
	exp := NewSnapshotExporter(svc, ExporterConfig{Bucket: bucket, NodeID: "n"}, newTestLogger())

	exp.exportFullAll(context.Background())
	ok, err := bucket.Exists(context.Background(), FullMetaKey("orders", 7))
	require.NoError(t, err)
	assert.False(t, ok, "a follower must not upload full snapshots")

	svc.leader = true
	exp.exportFullAll(context.Background())
	ok, err = bucket.Exists(context.Background(), FullMetaKey("orders", 7))
	require.NoError(t, err)
	assert.True(t, ok)
}
