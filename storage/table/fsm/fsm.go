// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"io"
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
	maxUserKey = mustEncodeKey(key.Key{
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
