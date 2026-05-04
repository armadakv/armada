// Copyright JAMF Software, LLC

package pebble

import (
	"encoding/binary"

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

	// v2SeqLen is the number of bytes used to encode the MVCC seqno at the
	// tail of every V2 physical key.
	v2SeqLen = 8
)

// mvccSeqnoMapper implements sstable.IntervalMapper. For every V2 user key it
// decodes the MVCC seqno from the trailing 8 bytes of the physical key and
// maps it to the half-open interval [seqno, seqno+1). System keys and
// non-V2 keys are ignored (empty interval).
type mvccSeqnoMapper struct{}

// MapPointKey implements sstable.IntervalMapper.
func (m mvccSeqnoMapper) MapPointKey(key sstable.InternalKey, _ []byte) (sstable.BlockInterval, error) {
	uk := key.UserKey
	// Minimum meaningful V2 key:
	//   header(4) + keyType(1) + sep(1) + seqno(8) = 14 bytes.
	// We only require len > v2SeqLen to be safe.
	if len(uk) <= v2SeqLen || uk[0] != v2KeyVersion {
		return sstable.BlockInterval{}, nil
	}
	seqno := decodeV2Seqno(uk)
	return sstable.BlockInterval{Lower: seqno, Upper: seqno + 1}, nil
}

// MapRangeKeys implements sstable.IntervalMapper. Range keys are not MVCC
// versioned in our schema, so we return an empty interval.
func (m mvccSeqnoMapper) MapRangeKeys(_ sstable.Span) (sstable.BlockInterval, error) {
	return sstable.BlockInterval{}, nil
}

// decodeV2Seqno recovers the original uint64 seqno from the last v2SeqLen
// bytes of a physical V2 key. The bytes are stored as big-endian with every
// bit inverted so that higher seqnos sort before lower ones.
func decodeV2Seqno(key []byte) uint64 {
	var buf [v2SeqLen]byte
	copy(buf[:], key[len(key)-v2SeqLen:])
	for i := range buf {
		buf[i] = ^buf[i]
	}
	return binary.BigEndian.Uint64(buf[:])
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
