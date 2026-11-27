// Copyright JAMF Software, LLC

package pebble

import (
	tablekey "github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
)

const (
	// mvccSeqnoPropertyName is the name of the block property that records the
	// [min, max) interval of MVCC seqnos present in each SST block and table.
	// This name must be stable — it is persisted in SST metadata.
	mvccSeqnoPropertyName = "armada.mvcc.seqno"

	// v2KeyVersion is the first byte of a V2 physical key header.
	v2KeyVersion = byte(0x02)

	// v2SeqLen is the number of bytes used to encode the inverted MVCC seqno.
	v2SeqLen       = 8
	v2CreateSeqLen = 8
	v2KindLen      = 1
)

// mvccSeqnoMapper implements sstable.IntervalMapper. For every V2 user key it
// decodes the MVCC seqno before the creation metadata and record kind and
// maps it to the half-open interval [seqno, seqno+1). System keys and
// non-V2 keys are ignored (empty interval).
type mvccSeqnoMapper struct{}

// MapPointKey implements sstable.IntervalMapper.
func (m mvccSeqnoMapper) MapPointKey(ikey sstable.InternalKey, _ []byte) (sstable.BlockInterval, error) {
	uk := ikey.UserKey
	// Minimum meaningful V2 key includes its complete metadata suffix.
	if len(uk) < 4+1+1+v2SeqLen+v2CreateSeqLen+v2KindLen || uk[0] != v2KeyVersion {
		return sstable.BlockInterval{}, nil
	}
	decoded, err := tablekey.DecodeBytes(uk)
	if err != nil {
		return sstable.BlockInterval{}, err
	}
	return sstable.BlockInterval{Lower: decoded.Seqno, Upper: decoded.Seqno + 1}, nil
}

// MapRangeKeys implements sstable.IntervalMapper. Range keys are not MVCC
// versioned in our schema, so we return an empty interval.
func (m mvccSeqnoMapper) MapRangeKeys(_ sstable.Span) (sstable.BlockInterval, error) {
	return sstable.BlockInterval{}, nil
}

// NewMVCCSeqnoCollector returns a BlockPropertyCollector factory function
// suitable for use in pebble.Options.BlockPropertyCollectors. Each SST block
// and table will be annotated with the [min, max) interval of MVCC seqnos
// present in its V2 user keys.
func NewMVCCSeqnoCollector() func() pebble.BlockPropertyCollector {
	return func() pebble.BlockPropertyCollector {
		return sstable.NewBlockIntervalCollector(
			mvccSeqnoPropertyName,
			mvccSeqnoMapper{},
			nil, // no suffix replacement needed
		)
	}
}

// NewMVCCSeqnoFilter returns a BlockPropertyFilter that, when passed in
// IterOptions.PointKeyFilters, causes pebble to skip any SST block or table
// whose recorded seqno interval does not intersect [0, gcIndex). In other
// words, blocks where every key has seqno >= gcIndex are skipped entirely
// during a GC sweep — they contain nothing the GC needs to touch.
//
// The filter uses the half-open interval [lower, upper):
//   - lower = 0         → include all seqnos from 0 upward
//   - upper = gcIndex   → exclude blocks whose minimum seqno >= gcIndex
//
// A block is kept when its recorded [blockMin, blockMax) intersects [0, gcIndex),
// i.e. when blockMin < gcIndex. Blocks where blockMin >= gcIndex (all keys are
// above the horizon) are skipped.
func NewMVCCSeqnoFilter(gcIndex uint64) pebble.BlockPropertyFilter {
	return sstable.NewBlockIntervalFilter(
		mvccSeqnoPropertyName,
		0,       // lower bound (inclusive): include seqno 0 and above
		gcIndex, // upper bound (exclusive): skip blocks with min seqno >= gcIndex
		nil,     // no suffix replacement
	)
}

// NewMVCCSeqnoRangeFilter returns a BlockPropertyFilter that skips any SST
// block or table whose recorded seqno interval does not intersect the
// half-open interval [lower, upper). The Watch API uses it to read the
// versions written in a window of revisions without visiting older data.
//
// Blocks without the property, such as blocks written before it existed,
// are never skipped, and memtables are not filtered: callers must still check
// the seqno of every key they read.
func NewMVCCSeqnoRangeFilter(lower, upper uint64) pebble.BlockPropertyFilter {
	return sstable.NewBlockIntervalFilter(mvccSeqnoPropertyName, lower, upper, nil)
}

// WithMVCCBlockPropertyCollector adds the MVCC seqno block property collector
// to the pebble options, enabling per-block seqno range tracking in all SSTs
// written by this database instance.
func WithMVCCBlockPropertyCollector() Option {
	return &funcOption{func(options *pebble.Options) {
		options.BlockPropertyCollectors = append(
			options.BlockPropertyCollectors,
			NewMVCCSeqnoCollector(),
		)
	}}
}
