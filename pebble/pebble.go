// Copyright JAMF Software, LLC

package pebble

import (
	"fmt"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
)

const (
	// writeBufferSize inmemory write buffer size.
	writeBufferSize = 16 * 1024 * 1024
	// maxWriteBufferNumber number of write buffers.
	maxWriteBufferNumber = 4
	// maxOpenFiles number of max open files per pebble instance.
	maxOpenFiles = 1000
	// l0FileNumCompactionTrigger number of files in L0 to trigger automatic compaction.
	l0FileNumCompactionTrigger = 8
	// l0StopWritesTrigger number of files in L0 to stop accepting more writes.
	l0StopWritesTrigger = 256
	// maxBytesForLevelBase base for amount of data stored in a single level.
	maxBytesForLevelBase = 64 * 1024 * 1024
)

func split(b []byte) int {
	return len(b)
}

func DefaultOptions() *pebble.Options {
	opts := &pebble.Options{
		FormatMajorVersion:          pebble.FormatValueSeparation,
		L0CompactionFileThreshold:   l0FileNumCompactionTrigger,
		L0StopWritesThreshold:       l0StopWritesTrigger,
		LBaseMaxBytes:               maxBytesForLevelBase,
		MemTableSize:                writeBufferSize,
		MemTableStopWritesThreshold: maxWriteBufferNumber,
		MaxOpenFiles:                maxOpenFiles,
		Comparer: &pebble.Comparer{
			Compare:            pebble.DefaultComparer.Compare,
			Equal:              pebble.DefaultComparer.Equal,
			AbbreviatedKey:     pebble.DefaultComparer.AbbreviatedKey,
			FormatKey:          pebble.DefaultComparer.FormatKey,
			FormatValue:        pebble.DefaultComparer.FormatValue,
			Separator:          pebble.DefaultComparer.Separator,
			Split:              split,
			Successor:          pebble.DefaultComparer.Successor,
			ImmediateSuccessor: pebble.DefaultComparer.ImmediateSuccessor,
			Name:               pebble.DefaultComparer.Name,
		},
	}
	opts.Levels[0] = pebble.LevelOptions{
		BlockSize:      32 << 10,  // 32 KB
		IndexBlockSize: 256 << 10, // 256 KB
		FilterPolicy:   bloom.FilterPolicy(10),
	}
	opts.Levels[0].EnsureL0Defaults()
	for i := 1; i < len(opts.Levels); i++ {
		l := &opts.Levels[i]
		l.BlockSize = 32 << 10       // 32 KB
		l.IndexBlockSize = 256 << 10 // 256 KB
		l.FilterPolicy = bloom.FilterPolicy(10)
		l.EnsureL1PlusDefaults(&opts.Levels[i-1])
	}
	opts.AllocatorSizeClasses = []int{
		16384,
		20480, 24576, 28672, 32768,
		40960, 49152, 57344, 65536,
		81920, 98304, 114688, 131072,
	}
	opts.EnsureDefaults()
	opts.Experimental.EnableValueBlocks = func() bool { return true }
	opts.Experimental.IngestSplit = func() bool { return true }
	opts.Experimental.ValueSeparationPolicy = func() pebble.ValueSeparationPolicy {
		return pebble.ValueSeparationPolicy{
			Enabled:               true,
			MinimumSize:           256,
			MaxBlobReferenceDepth: 10,
			RewriteMinimumAge:     5 * time.Minute,
			TargetGarbageRatio:    10 / 100,
		}
	}
	return opts
}

func WriterOptions(level int) sstable.WriterOptions {
	return DefaultOptions().MakeWriterOptions(level, sstable.TableFormatPebblev4)
}

type Option interface {
	apply(options *pebble.Options)
}

type funcOption struct {
	f func(options *pebble.Options)
}

func (fdo *funcOption) apply(do *pebble.Options) {
	fdo.f(do)
}

func WithFS(fs vfs.FS) Option {
	return &funcOption{func(options *pebble.Options) {
		options.FS, _ = vfs.WithDiskHealthChecks(fs, 5*time.Second, nil, func(info pebble.DiskSlowInfo) {
			if options.EventListener != nil {
				options.EventListener.DiskSlow(info)
			}
		})
	}}
}

func WithCache(cache *pebble.Cache) Option {
	return &funcOption{func(options *pebble.Options) {
		options.Cache = cache
	}}
}

func WithCompactionScheduler(scheduler pebble.CompactionScheduler) Option {
	return &funcOption{func(options *pebble.Options) {
		options.Experimental.CompactionScheduler = scheduler
	}}
}

func WithLogger(logger pebble.Logger) Option {
	return &funcOption{func(options *pebble.Options) {
		options.Logger = logger
	}}
}

func WithEventListener(listener pebble.EventListener) Option {
	return &funcOption{func(options *pebble.Options) {
		options.AddEventListener(listener)
	}}
}

// OpenDB opens DB on paths given (using sane defaults).
func OpenDB(dbdir string, options ...Option) (*pebble.DB, error) {
	opts := DefaultOptions()
	for _, option := range options {
		option.apply(opts)
	}
	db, err := pebble.Open(dbdir, opts)
	if err != nil {
		return nil, fmt.Errorf("error opening DB: %w", err)
	}
	err = db.RatchetFormatMajorVersion(pebble.FormatValueSeparation)
	if err != nil {
		return nil, fmt.Errorf("error ratcheting DB: %w", err)
	}
	return db, nil
}
