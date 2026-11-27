// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"encoding/binary"
	stderrors "errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/armadakv/armada/armadapb"
	rp "github.com/armadakv/armada/pebble"
	sm "github.com/armadakv/armada/raft/statemachine"
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
	// sysRevision is the table revision: the source sequence of the last
	// applied write. Unlike sysLocalIndex it is the same on every cluster, and
	// unlike sysLeaderIndex it is not written by locally proposed GC commands.
	sysRevision = mustEncodeKey(key.Key{
		KeyType: key.TypeSystem,
		Key:     []byte("revision"),
	})
	// minEncodedUserKey uses the highest logical seqno, whose inverted physical
	// suffix is all zeroes, so every physical version sorts at or above it.
	minEncodedUserKey = mustEncodeKey(key.Key{
		KeyType: key.TypeUser,
		Key:     key.LatestMinKey,
		Seqno:   ^uint64(0),
	})
	maxEncodedUserKey = mustEncodeKey(key.Key{
		KeyType: key.TypeUser,
		Key:     key.LatestMaxKey,
	})
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

// snapshotFormat identifies the wire format of an intra-cluster Raft snapshot.
// The values are part of the snapshot header and must not be renumbered.
type snapshotFormat uint8

const (
	// snapshotFormatLegacy re-encoded the key space into SSTs. It is no longer
	// produced or accepted; the value stays reserved so that such a snapshot is
	// rejected instead of being misread.
	snapshotFormatLegacy snapshotFormat = iota
	// snapshotFormatCheckpoint is a tarball of a Pebble checkpoint.
	snapshotFormatCheckpoint
)

// snapshotHeader first 8 bytes of a snapshot is this header.
// layout:
// 0-5 reserved for extension
// 6 snapshot format
// 7 sentinel byte.
type snapshotHeader [8]byte

func (s *snapshotHeader) setFormat(format snapshotFormat) {
	s[6] = byte(format)
}

func (s *snapshotHeader) format() snapshotFormat {
	return snapshotFormat(s[6])
}

// New returns a factory of table state machines. observe, when set, returns
// the Observer notified about the committed batches of the given shard.
func New(tableName, stateMachineDir string, fs vfs.FS, blockCache *pebble.Cache, compactionScheduler *rp.ConcurrencyLimitScheduler, af func(applied uint64), observe func(clusterID uint64) Observer) sm.CreateOnDiskStateMachineFunc {
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
			appliedFunc:         af,
			observe:             observe,
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
	compactionScheduler *rp.ConcurrencyLimitScheduler
	metrics             *metrics
	appliedFunc         func(applied uint64)
	// observe returns the observer of a shard. It is called on every Open, so a
	// shard restarted on this node never reuses the observer of the instance
	// that closed before it.
	observe func(clusterID uint64) Observer
	// observer is notified about committed batches.
	observer Observer
	// revision is the table revision, see sysRevision.
	revision atomic.Uint64

	// gcHorizon is the MVCC seqno below which GC may remove versions that a
	// newer version shadows. It never decreases.
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
	installed := false
	defer func() {
		if !installed {
			_ = db.Close()
		}
	}()

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

	rev, err := readRevision(db)
	if err != nil {
		return 0, err
	}
	p.revision.Store(rev)
	if p.observe != nil {
		p.observer = p.observe(p.clusterID)
	}

	p.pebble.Store(db)
	if err := prometheus.Register(p); err != nil {
		p.log.Errorf("unable to register metrics for FSM: %s", err)
	}

	// Start background GC worker.
	p.startGCWorker()
	installed = true
	p.getObserver().Committed(Commit{Revision: rev})

	return idx, nil
}

func (p *FSM) openDB(dbdir string) (*pebble.DB, error) {
	if p.compactionScheduler == nil {
		p.compactionScheduler = rp.NewConcurrencyLimitScheduler()
	}
	return rp.OpenDB(
		dbdir,
		rp.WithFS(p.fs),
		rp.WithCache(p.blockCache),
		rp.WithCompactionScheduler(p.compactionScheduler.NewScheduler()),
		rp.WithLogger(p.log),
		rp.WithEventListener(makeLoggingEventListener(p.log)),
		rp.WithMVCCBlockPropertyCollector(),
	)
}

// Lookup locally looks up the data.
func (p *FSM) Lookup(l any) (any, error) {
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
		return &SnapshotResponse{Index: idx, TipIndex: idx}, nil
	case IncrementalSnapshotRequest:
		snapshot := p.pebble.Load().NewSnapshot()
		defer snapshot.Close()

		resp, err := commandIncrementalSnapshot(snapshot, p.tableName, req.SinceIndex, req.Writer, req.Stopper)
		if err != nil {
			return nil, err
		}
		return resp, nil
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
		batch:     db.NewBatch(pebble.WithInitialSizeBytes(updatesSize(updates))),
		db:        db,
		gcHorizon: p.gcHorizon.Load(),
		revision:  p.revision.Load(),
	}
	observer := p.getObserver()
	if observer.Capturing() {
		ctx.events = newEventRecorder()
	}

	defer func() {
		_ = ctx.Close()
	}()

	var idx uint64
	for i := range updates {
		cmd, err := parseCommand(ctx, updates[i])
		if err != nil {
			return nil, err
		}

		updateResult, res, err := cmd.handle(ctx)
		if err != nil {
			return nil, err
		}
		ctx.advanceRevision()

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
	p.revision.Store(ctx.revision)
	observer.Committed(ctx.commit())

	// Advance the in-memory GC horizon and wake the sweep worker when a GC
	// command advanced it, including one nested in a replicated SEQUENCE.
	if ctx.gcHorizonChanged {
		p.gcHorizon.Store(ctx.gcHorizon)
		p.signalGCWorker()
	}

	p.metrics.applied.Store(idx)
	switch {
	case !ctx.nonGCApplied:
		// Only GC was applied: there is no new leader index to report, and
		// the compacted index a GC carries is not one.
	case ctx.appliedLeaderIndex != nil:
		p.appliedFunc(*ctx.appliedLeaderIndex)
	default:
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
	defer p.getObserver().Closed()

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
	db := p.pebble.Load()
	gcH, _ := readLocalIndex(db, sysGCHorizon)
	p.gcHorizon.Store(gcH)
	p.signalGCWorker()

	rev, err := readRevision(db)
	if err != nil {
		p.log.Errorf("unable to read the revision of the recovered state: %v", err)
		return
	}
	p.revision.Store(rev)
	// The recovered state carries the sender's complete history, so watchers
	// can continue by reading the skipped revisions from the store.
	p.getObserver().Committed(Commit{Revision: rev})
}

// getObserver returns the observer, or a no-op one when nobody observes.
func (p *FSM) getObserver() Observer {
	if p.observer == nil {
		return noopObserver{}
	}
	return p.observer
}

// readRevision reads the table revision. State written before the revision
// was tracked has no such key: it falls back to the source index, which is
// exact on follower clusters and at most the compacted index on the leader
// cluster, and otherwise to the local index. The fallback only applies until
// the next write persists the revision.
func readRevision(db pebble.Reader) (uint64, error) {
	val, closer, err := db.Get(sysRevision)
	switch {
	case err == nil:
		defer func() { _ = closer.Close() }()
		return binary.LittleEndian.Uint64(val), nil
	case !stderrors.Is(err, pebble.ErrNotFound):
		return 0, err
	}
	if lx, err := readLocalIndex(db, sysLeaderIndex); err != nil || lx != 0 {
		return lx, err
	}
	return readLocalIndex(db, sysLocalIndex)
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
func (p *FSM) PrepareSnapshot() (any, error) {
	return (&checkpoint{p}).prepare()
}

// SaveSnapshot saves the state of the object to the provided io.Writer object.
func (p *FSM) SaveSnapshot(ctx any, w io.Writer, stopc <-chan struct{}) error {
	c := &checkpoint{p}
	if err := binary.Write(w, binary.LittleEndian, c.getHeader()); err != nil {
		return err
	}
	return c.save(ctx, w, stopc)
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
	if format := header.format(); format != snapshotFormatCheckpoint {
		return fmt.Errorf("%w: %d", errors.ErrUnsupportedSnapshotFormat, format)
	}
	return (&checkpoint{p}).recover(r, stopc)
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

// encodeUserKeyPrefix writes the upper-bound user key used to scan all versions of a key.
func encodeUserKeyPrefix(dst io.Writer, keyBytes []byte) error {
	return encodeUserRecord(dst, keyBytes, ^uint64(0), 0, key.KindValue)
}

func encodeUserRecord(dst io.Writer, keyBytes []byte, seqno, createSeqno uint64, kind key.RecordKind) error {
	enc := key.NewEncoder(dst)
	k := &key.Key{
		KeyType:     key.TypeUser,
		Key:         keyBytes,
		Seqno:       seqno,
		CreateSeqno: createSeqno,
		Kind:        kind,
	}

	if _, err := enc.Encode(k); err != nil {
		return err
	}

	return nil
}

func validateRevision(seqno uint64) error {
	if seqno > math.MaxInt64 {
		return fmt.Errorf("revision %d exceeds the maximum supported revision", seqno)
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
