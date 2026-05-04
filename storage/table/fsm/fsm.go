// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"encoding/binary"
	stderrors "errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	rp "github.com/armadakv/armada/pebble"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/errors"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/oxtoacart/bpool"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

var (
	bufferPool    = bpool.NewSizedBufferPool(256, 128)
	wildcard      = []byte{0}
	sysLocalIndex = mustEncodeKey(key.Key{
		KeyType: key.TypeSystem,
		Key:     []byte("index"),
	})
	sysLeaderIndex = mustEncodeKey(key.Key{
		KeyType: key.TypeSystem,
		Key:     []byte("leader_index"),
	})
	sysGCHorizon = mustEncodeKey(key.Key{
		KeyType: key.TypeSystem,
		Key:     []byte("gc_horizon"),
	})
	maxUserKey = mustEncodeKey(key.Key{
		KeyType: key.TypeUser,
		Key:     key.LatestMaxKey,
	})
)

const (
	// maxBatchSize maximum size of inmemory batch before commit.
	maxBatchSize = 16 * 1024 * 1024
)

// UpdateResult if operation succeeded or not, both values mean that operation finished, value just indicates with which result.
// You should always check for err from proposals to detect unfinished or failed operations.
type UpdateResult uint64

const (
	// ResultFailure failed to apply update.
	ResultFailure UpdateResult = iota
	// ResultSuccess applied update.
	ResultSuccess
	// ResultGC applied a GC horizon advance.
	ResultGC
)

type SnapshotRecoveryType uint8

const (
	RecoveryTypeSnapshot SnapshotRecoveryType = iota
	RecoveryTypeCheckpoint
)

type snapshotRecoverer interface {
	prepare() (any, error)
	getHeader() snapshotHeader
	save(ctx any, w io.Writer, stopc <-chan struct{}) error
	recover(r io.Reader, stopc <-chan struct{}) error
}

// snapshotHeader first 8 bytes of a snapshot is this header.
// layout:
// 0-5 reserved for extension
// 6 snapshot format
// 7 sentinel byte.
type snapshotHeader [8]byte

func (s *snapshotHeader) setSnapshotType(recoveryType SnapshotRecoveryType) {
	s[6] = byte(recoveryType)
}

func (s *snapshotHeader) snapshotType() SnapshotRecoveryType {
	return SnapshotRecoveryType(s[6])
}

func New(tableName, stateMachineDir string, fs vfs.FS, blockCache *pebble.Cache, compactionScheduler pebble.CompactionScheduler, srt SnapshotRecoveryType, af func(applied uint64)) sm.CreateOnDiskStateMachineFunc {
	if fs == nil {
		fs = vfs.Default
	}
	if af == nil {
		af = func(applied uint64) {}
	}
	return func(clusterID uint64, nodeID uint64) sm.IOnDiskStateMachine {
		hostname, _ := os.Hostname()
		dbDirName := rp.GetNodeDBDirName(stateMachineDir, hostname, fmt.Sprintf("%s-%d", tableName, clusterID))

		return &FSM{
			tableName:           tableName,
			clusterID:           clusterID,
			nodeID:              nodeID,
			dirname:             dbDirName,
			fs:                  fs,
			blockCache:          blockCache,
			compactionScheduler: compactionScheduler,
			log:                 zap.S().Named("table").Named(tableName),
			metrics:             newMetrics(tableName, clusterID),
			recoveryType:        srt,
			appliedFunc:         af,
		}
	}
}

// FSM is a statemachine.IOnDiskStateMachine impl.
type FSM struct {
	pebble              atomic.Pointer[pebble.DB]
	fs                  vfs.FS
	clusterID           uint64
	nodeID              uint64
	tableName           string
	dirname             string
	closed              bool
	log                 *zap.SugaredLogger
	blockCache          *pebble.Cache
	compactionScheduler pebble.CompactionScheduler
	metrics             *metrics
	recoveryType        SnapshotRecoveryType
	appliedFunc         func(applied uint64)

	// gcHorizon is the highest leaderIndex up to which MVCC GC is safe.
	// It is persisted as sysGCHorizon and kept in sync via this atomic.
	gcHorizon atomic.Uint64
	// gcCh is a buffered(1) channel used to signal the GC worker to run a sweep immediately.
	gcCh   chan struct{}
	gcStop chan struct{}
	gcDone chan struct{}
}

func (p *FSM) Open(_ <-chan struct{}) (uint64, error) {
	p.gcCh = make(chan struct{}, 1)
	p.gcStop = make(chan struct{})
	p.gcDone = make(chan struct{})

	if p.clusterID < 1 {
		return 0, errors.ErrInvalidClusterID
	}
	if p.nodeID < 1 {
		return 0, errors.ErrInvalidNodeID
	}

	if err := rp.CreateNodeDataDir(p.fs, p.dirname); err != nil {
		return 0, err
	}

	randomDir := rp.GetNewRandomDBDirName()
	var dbdir string
	if rp.IsNewRun(p.fs, p.dirname) {
		dbdir = filepath.Join(p.dirname, randomDir)
		if err := rp.SaveCurrentDBDirName(p.fs, p.dirname, randomDir); err != nil {
			return 0, err
		}
		if err := rp.ReplaceCurrentDBFile(p.fs, p.dirname); err != nil {
			return 0, err
		}
	} else {
		if err := rp.CleanupNodeDataDir(p.fs, p.dirname); err != nil {
			return 0, err
		}
		var err error
		randomDir, err = rp.GetCurrentDBDirName(p.fs, p.dirname)
		if err != nil {
			return 0, err
		}
		dbdir = filepath.Join(p.dirname, randomDir)
		if _, err := p.fs.Stat(filepath.Join(p.dirname, randomDir)); err != nil {
			return 0, err
		}
	}

	p.log.Infof("opening pebble state machine with dirname: '%s'", dbdir)
	db, err := p.openDB(dbdir)
	if err != nil {
		return 0, err
	}
	p.pebble.Store(db)

	if err := prometheus.Register(p); err != nil {
		p.log.Errorf("unable to register metrics for FSM: %s", err)
	}

	idx, err := readLocalIndex(db, sysLocalIndex)
	if err != nil {
		return 0, err
	}
	p.metrics.applied.Store(idx)
	p.appliedFunc(idx)
	lx, _ := readLocalIndex(db, sysLeaderIndex)
	if lx != 0 {
		p.appliedFunc(lx)
	}

	// Restore GC horizon from durable storage.
	gcH, _ := readLocalIndex(db, sysGCHorizon)
	p.gcHorizon.Store(gcH)

	// Start background GC worker.
	p.startGCWorker()

	return idx, nil
}

func (p *FSM) openDB(dbdir string) (*pebble.DB, error) {
	return rp.OpenDB(
		dbdir,
		rp.WithFS(p.fs),
		rp.WithCache(p.blockCache),
		rp.WithCompactionScheduler(p.compactionScheduler),
		rp.WithLogger(p.log),
		rp.WithEventListener(makeLoggingEventListener(p.log)),
		rp.WithMVCCBlockPropertyCollector(),
	)
}

// runGC performs an explicit MVCC garbage collection sweep up to the given
// raft index. It iterates every physical key in the user keyspace and deletes:
//   - any version with seqno < gcIndex that is shadowed by a newer version
//     of the same logical user key (i.e. the same key prefix), and
//   - any tombstone version with seqno < gcIndex that has no newer live
//     version above gcIndex.
//
// Deletions are batched; when the batch reaches maxBatchSize it is committed
// and a new one is opened. After all obsolete versions are removed, pebble's
// Compact is called over the full user keyspace so the deleted entries are
// physically reclaimed from disk.
func (p *FSM) runGC(db *pebble.DB, gcIndex uint64) error {
	if gcIndex == 0 {
		return nil
	}

	// Scan the entire user keyspace using a snapshot so the view is stable
	// for the duration of the sweep.
	snap := db.NewSnapshot()
	defer snap.Close()

	// The MVCC seqno block property filter lets pebble skip entire SST blocks
	// whose recorded seqno interval does not intersect [0, gcIndex). Blocks
	// where every key has seqno >= gcIndex contain nothing the GC can touch and
	// are skipped without even being opened, dramatically reducing I/O for
	// databases with mostly-recent data.
	seqnoFilter := rp.NewMVCCSeqnoFilter(gcIndex)
	iter, err := snap.NewIter(&pebble.IterOptions{
		LowerBound:      mustEncodeKey(key.Key{KeyType: key.TypeUser, Key: key.LatestMinKey}),
		UpperBound:      incrementRightmostByte(append([]byte(nil), maxUserKey...)),
		PointKeyFilters: []pebble.BlockPropertyFilter{seqnoFilter},
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	batch := db.NewBatch(pebble.WithInitialSizeBytes(maxBatchSize))

	// lastPrefix holds the user-key prefix (physical key minus the seqno
	// suffix) of the most recently visited entry. Because pebble iterates in
	// ascending key order and within the same prefix in descending seqno
	// order (latest first), the first time we see a prefix is the newest
	// version — all subsequent visits to the same prefix are older versions
	// and are therefore shadowed.
	var lastPrefix []byte
	var lastPrefixIsTombstone bool

	commitBatch := func() error {
		if batch.Count() == 0 {
			return nil
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return err
		}
		batch = db.NewBatch(pebble.WithInitialSizeBytes(maxBatchSize))
		return nil
	}

	for iter.First(); iter.Valid(); iter.Next() {
		physKey := iter.Key()

		// Decode the logical key to get keyType and seqno.
		k, err := key.DecodeBytes(physKey)
		if err != nil {
			return err
		}
		// Only GC user keys; system keys have no MVCC versions.
		if k.KeyType != key.TypeUser {
			continue
		}

		// Prefix = everything except the trailing seqno bytes.
		prefixLen := len(physKey) - key.V2SeqLen
		prefix := physKey[:prefixLen]

		sameAsLast := bytes.Equal(lastPrefix, prefix)

		if k.Seqno >= gcIndex {
			// This version is at or above the safe horizon — always keep it.
			// Update prefix tracking. lastPrefixIsTombstone is intentionally
			// not set here — it is only meaningful for the first version below
			// gcIndex (the else branch below), where it decides whether to keep
			// or discard a live-but-old value.
			if !sameAsLast {
				lastPrefix = append(lastPrefix[:0], prefix...)
			}
			continue
		}

		// Version is below gcIndex.
		if sameAsLast {
			// Shadowed by the newer version we already decided to keep (or
			// already GC'd). Discard regardless.
			physKeyCopy := make([]byte, len(physKey))
			copy(physKeyCopy, physKey)
			if err := batch.Delete(physKeyCopy, nil); err != nil {
				return err
			}
		} else {
			// First (newest) version of this prefix below gcIndex.
			lastPrefix = append(lastPrefix[:0], prefix...)
			lastPrefixIsTombstone = isTombstone(iter.Value())

			if lastPrefixIsTombstone {
				// Tombstone with no live version above gcIndex — discard.
				physKeyCopy := make([]byte, len(physKey))
				copy(physKeyCopy, physKey)
				if err := batch.Delete(physKeyCopy, nil); err != nil {
					return err
				}
			}
			// Live value below gcIndex with no newer version — keep it.
		}

		if uint64(batch.Len()) >= maxBatchSize {
			if err := commitBatch(); err != nil {
				return err
			}
			time.Sleep(gcThrottleSleep)
		}
	}

	if err := commitBatch(); err != nil {
		return err
	}

	return nil
}

// Lookup locally looks up the data.
func (p *FSM) Lookup(l interface{}) (interface{}, error) {
	switch req := l.(type) {
	case *armadapb.TxnRequest:
		snapshot := p.pebble.Load().NewSnapshot()
		defer snapshot.Close()

		ok, err := txnCompare(snapshot, req.Compare)
		if err != nil {
			return nil, err
		}

		var ops []*armadapb.RequestOp_Range
		if ok {
			for _, op := range req.Success {
				ops = append(ops, op.GetRequestRange())
			}
		} else {
			for _, op := range req.Failure {
				ops = append(ops, op.GetRequestRange())
			}
		}

		resp := &armadapb.TxnResponse{Succeeded: ok}
		for _, op := range ops {
			rr, err := lookup(snapshot, op)
			if err != nil {
				return nil, err
			}
			resp.Responses = append(resp.Responses, wrapResponseOp(rr))
		}
		return resp, nil
	case *armadapb.RequestOp_Range:
		db := p.pebble.Load()
		return lookup(db, req)
	case IteratorRequest:
		db := p.pebble.Load()
		return iteratorLookup(db, req.RangeOp)
	case SnapshotRequest:
		snapshot := p.pebble.Load().NewSnapshot()
		defer snapshot.Close()

		idx, err := commandSnapshot(snapshot, p.tableName, req.Writer, req.Stopper)
		if err != nil {
			return nil, err
		}
		return &SnapshotResponse{Index: idx}, nil
	case IncrementalSnapshotRequest:
		snapshot := p.pebble.Load().NewSnapshot()
		defer snapshot.Close()

		idx, err := commandIncrementalSnapshot(snapshot, p.tableName, req.SinceIndex, req.Writer, req.Stopper)
		if err != nil {
			return nil, err
		}
		return &SnapshotResponse{Index: idx}, nil
	case LocalIndexRequest:
		idx, err := readLocalIndex(p.pebble.Load(), sysLocalIndex)
		if err != nil {
			return nil, err
		}
		return &IndexResponse{Index: idx}, nil
	case LeaderIndexRequest:
		idx, err := readLocalIndex(p.pebble.Load(), sysLeaderIndex)
		if err != nil {
			return nil, err
		}
		return &IndexResponse{Index: idx}, nil
	case PathRequest:
		return &PathResponse{Path: p.dirname}, nil
	case GCHorizonRequest:
		return &IndexResponse{Index: p.gcHorizon.Load()}, nil
	default:
		p.log.Warnf("received unknown lookup request of type %T", req)
	}

	return nil, errors.ErrUnknownQueryType
}

// Update advances the FSM.
func (p *FSM) Update(updates []sm.Entry) ([]sm.Entry, error) {
	db := p.pebble.Load()

	ctx := &updateContext{
		batch: db.NewBatch(pebble.WithInitialSizeBytes(updatesSize(updates))),
		db:    db,
	}

	defer func() {
		_ = ctx.Close()
	}()

	var idx uint64
	var gcHorizonApplied uint64
	for i := 0; i < len(updates); i++ {
		cmd, err := parseCommand(ctx, updates[i])
		if err != nil {
			return nil, err
		}

		updateResult, res, err := cmd.handle(ctx)
		if err != nil {
			return nil, err
		}

		if updateResult == ResultGC {
			gcHorizonApplied = res.Revision
		}

		bts, err := res.MarshalVT()
		if err != nil {
			return nil, err
		}
		updates[i].Result.Data = bts
		updates[i].Result.Value = uint64(updateResult)
		idx = updates[i].Index
	}

	if err := ctx.Commit(); err != nil {
		return nil, err
	}

	// Advance in-memory GC horizon and wake the sweep worker if a GC command was applied.
	if gcHorizonApplied > 0 {
		p.gcHorizon.Store(gcHorizonApplied)
		p.signalGCWorker()
	}

	p.metrics.applied.Store(idx)
	if ctx.leaderIndex != nil {
		p.appliedFunc(*ctx.leaderIndex)
	} else {
		p.appliedFunc(idx)
	}
	return updates, nil
}

// Sync synchronizes all in-core state of the state machine to permanent
// storage so the state machine can continue from its latest state after
// reboot.
func (p *FSM) Sync() error {
	return p.pebble.Load().Flush()
}

// Close closes the KVStateMachine IStateMachine.
func (p *FSM) Close() error {
	p.closed = true
	prometheus.Unregister(p)

	// Stop GC worker before closing pebble so any in-progress sweep can
	// observe the closed DB and exit cleanly.
	if p.gcStop != nil {
		close(p.gcStop)
		<-p.gcDone
	}

	db := p.pebble.Load()
	if db == nil {
		return nil
	}
	if err := db.Flush(); err != nil {
		return err
	}
	return db.Close()
}

// notifyRecovered is called by snapshot recoverers after atomically swapping
// the pebble DB. It updates the in-memory GC horizon from the new DB and
// kicks the GC worker to sweep the freshly recovered state.
func (p *FSM) notifyRecovered() {
	gcH, _ := readLocalIndex(p.pebble.Load(), sysGCHorizon)
	p.gcHorizon.Store(gcH)
	p.signalGCWorker()
}

// signalGCWorker sends a non-blocking signal to the GC worker to start a sweep.
func (p *FSM) signalGCWorker() {
	select {
	case p.gcCh <- struct{}{}:
	default:
	}
}

const (
	gcWorkerPeriod  = 10 * time.Minute
	gcThrottleSleep = 5 * time.Millisecond // pause between batch commits
	gcThrottleBatch = 512                  // keys processed per batch before sleeping
)

// startGCWorker launches the background goroutine that performs throttled
// MVCC GC sweeps. It is started by Open() and stopped by Close().
func (p *FSM) startGCWorker() {
	go func() {
		defer close(p.gcDone)
		ticker := time.NewTicker(gcWorkerPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-p.gcStop:
				return
			case <-p.gcCh:
				p.runGCSweep()
			case <-ticker.C:
				p.runGCSweep()
			}
		}
	}()
}

// runGCSweep runs a single throttled MVCC GC sweep using the current gcHorizon.
func (p *FSM) runGCSweep() {
	horizon := p.gcHorizon.Load()
	if horizon == 0 {
		return
	}
	db := p.pebble.Load()
	if db == nil {
		return
	}
	if err := p.runGC(db, horizon); err != nil {
		if !stderrors.Is(err, pebble.ErrClosed) {
			p.log.Warnf("GC sweep at horizon %d failed: %v", horizon, err)
		}
	}
}

// GetHash gets the DB hash for test comparison.
func (p *FSM) GetHash() (uint64, error) {
	db := p.pebble.Load()
	snap := db.NewSnapshot()
	iter, err := snap.NewIter(nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err := iter.Close(); err != nil {
			p.log.Error(err)
		}
		if err := snap.Close(); err != nil {
			p.log.Error(err)
		}
	}()

	// Compute Hash
	hash64 := fnv.New64()
	// iterate through the whole kv space and send it to hash func
	for iter.First(); iter.Valid(); iter.Next() {
		_, err := hash64.Write(iter.Key())
		if err != nil {
			return 0, err
		}
		_, err = hash64.Write(iter.Value())
		if err != nil {
			return 0, err
		}
	}

	return hash64.Sum64(), nil
}

// PrepareSnapshot prepares the snapshot to be concurrently captured and
// streamed.
func (p *FSM) PrepareSnapshot() (interface{}, error) {
	return p.getRecoverer(p.recoveryType).prepare()
}

// SaveSnapshot saves the state of the object to the provided io.Writer object.
func (p *FSM) SaveSnapshot(ctx interface{}, w io.Writer, stopc <-chan struct{}) error {
	r := p.getRecoverer(p.recoveryType)
	if err := binary.Write(w, binary.LittleEndian, r.getHeader()); err != nil {
		return err
	}
	return r.save(ctx, w, stopc)
}

// RecoverFromSnapshot recovers the state machine state from snapshot specified by
// the io.Reader object. The snapshot is recovered into a new DB first and then
// atomically swapped with the existing DB to complete the recovery.
func (p *FSM) RecoverFromSnapshot(r io.Reader, stopc <-chan struct{}) error {
	var header snapshotHeader
	err := binary.Read(r, binary.LittleEndian, &header)
	if err != nil {
		return err
	}
	return p.getRecoverer(header.snapshotType()).recover(r, stopc)
}

func (p *FSM) Collect(ch chan<- prometheus.Metric) {
	if p.metrics == nil {
		return
	}
	db := p.pebble.Load()
	if db == nil {
		return
	}
	p.metrics.collected = db.Metrics()
	p.metrics.Collect(ch)
}

func (p *FSM) Describe(ch chan<- *prometheus.Desc) {
	if p.metrics == nil {
		return
	}
	p.metrics.Describe(ch)
}

func (p *FSM) getRecoverer(recoveryType SnapshotRecoveryType) snapshotRecoverer {
	switch recoveryType {
	case RecoveryTypeSnapshot:
		return &snapshot{p}
	case RecoveryTypeCheckpoint:
		return &checkpoint{p}
	default:
		panic(fmt.Sprintf("unknown recoverer type: %d", p.recoveryType))
	}
}

// encodeUserKey into provided writer.
func encodeUserKey(dst io.Writer, keyBytes []byte, seqno uint64) error {
	enc := key.NewEncoder(dst)
	k := &key.Key{
		KeyType: key.TypeUser,
		Key:     keyBytes,
		Seqno:   seqno,
	}

	if _, err := enc.Encode(k); err != nil {
		return err
	}

	return nil
}

func mustEncodeKey(k key.Key) []byte {
	// Pre-encode system keys
	buff := bytes.NewBuffer(make([]byte, 0))
	enc := key.NewEncoder(buff)
	n, err := enc.Encode(&k)
	if err != nil {
		panic(err)
	}
	encoded := make([]byte, n)
	copy(encoded, buff.Bytes())
	return encoded
}

func incrementRightmostByte(in []byte) []byte {
	for i := len(in) - 1; i >= 0; i-- {
		in[i] = in[i] + 1
		if in[i] != 0 {
			break
		}
		if i == 0 {
			return prependByte(in, 1)
		}
	}
	return in
}

func prependByte(x []byte, y byte) []byte {
	x = append(x, 0)
	copy(x[1:], x)
	x[0] = y
	return x
}

func makeLoggingEventListener(logger *zap.SugaredLogger) pebble.EventListener {
	logger = logger.WithOptions(zap.AddCallerSkip(1))
	return pebble.EventListener{
		BackgroundError: func(err error) {
			logger.Errorf("background error: %s", err)
		},
		CompactionBegin: func(info pebble.CompactionInfo) {
			logger.Debugf("%s", info)
		},
		CompactionEnd: func(info pebble.CompactionInfo) {
			logger.Infof("%s", info)
		},
		DiskSlow: func(info pebble.DiskSlowInfo) {
			logger.Warnf("%s", info)
		},
		FlushBegin: func(info pebble.FlushInfo) {
			logger.Debugf("%s", info)
		},
		FlushEnd: func(info pebble.FlushInfo) {
			logger.Debugf("%s", info)
		},
		ManifestCreated: func(info pebble.ManifestCreateInfo) {
			logger.Debugf("%s", info)
		},
		ManifestDeleted: func(info pebble.ManifestDeleteInfo) {
			logger.Debugf("%s", info)
		},
		TableCreated: func(info pebble.TableCreateInfo) {
			logger.Debugf("%s", info)
		},
		TableDeleted: func(info pebble.TableDeleteInfo) {
			logger.Debugf("%s", info)
		},
		TableIngested: func(info pebble.TableIngestInfo) {
			logger.Debugf("%s", info)
		},
		TableStatsLoaded: func(info pebble.TableStatsInfo) {
			logger.Debugf("%s", info)
		},
		WALCreated: func(info pebble.WALCreateInfo) {
			logger.Debugf("%s", info)
		},
		WALDeleted: func(info pebble.WALDeleteInfo) {
			logger.Debugf("%s", info)
		},
		WriteStallBegin: func(info pebble.WriteStallBeginInfo) {
			logger.Infof("%s", info)
		},
		WriteStallEnd: func() {
			logger.Debugf("write stall ending")
		},
	}
}

func updatesSize(updates []sm.Entry) int {
	size := 0
	for _, update := range updates {
		size += len(update.Cmd)
	}
	return size
}
