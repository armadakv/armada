// Copyright Armada Contributors

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/armadakv/objfs"
)

// ListMeta returns all committed snapshot metadata for tableName, sorted by
// TipIndex in ascending order.
func ListMeta(ctx context.Context, bucket objfs.Bucket, tableName string) ([]Meta, error) {
	prefix := fmt.Sprintf("snapshots/%s/", tableName)
	var metas []Meta
	err := bucket.List(ctx, prefix, func(a objfs.Attributes) error {
		name := a.Name
		if !strings.HasSuffix(name, ".meta") {
			return nil
		}
		r, err := bucket.Get(ctx, name)
		if err != nil {
			if errors.Is(err, objfs.ErrNotExist) {
				return nil
			}
			return err
		}
		defer r.Close()
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		var m Meta
		if err := unmarshalMeta(data, &m); err != nil {
			return nil
		}
		metas = append(metas, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(metas, func(i, j int) bool {
		return metas[i].TipIndex < metas[j].TipIndex
	})
	return metas, nil
}

// SnapshotSelectionOptions supplies the recovery facts needed to select an
// artefact. LiveGCHorizon is zero when the leader has not compacted.
type SnapshotSelectionOptions struct {
	FollowerIndex                  uint64
	LiveGCHorizon                  uint64
	RequireKnownIncrementalHorizon bool
	FollowerCanTail                bool
}

// SelectRecoverableSnapshot filters and ranks snapshot artefacts for recovery.
// A candidate must be resumable from its tip, applicable to the follower, and,
// when incremental, complete from its base. Among the remaining candidates, the
// one with the highest base index wins; ties use the highest tip index.
func SelectRecoverableSnapshot(metas []Meta, options SnapshotSelectionOptions) (Meta, bool) {
	if options.FollowerCanTail && options.LiveGCHorizon > 0 && options.FollowerIndex >= options.LiveGCHorizon {
		return Meta{}, false
	}

	var selected Meta
	found := false
	for _, meta := range metas {
		if options.LiveGCHorizon > 0 && meta.TipIndex < options.LiveGCHorizon {
			continue
		}
		if meta.TipIndex <= options.FollowerIndex || meta.BaseIndex > options.FollowerIndex {
			continue
		}
		if meta.Type == SnapshotTypeIncremental && !validIncremental(meta, options) {
			continue
		}
		if !found || meta.BaseIndex > selected.BaseIndex ||
			(meta.BaseIndex == selected.BaseIndex && meta.TipIndex > selected.TipIndex) {
			selected = meta
			found = true
		}
	}
	return selected, found
}

func validIncremental(meta Meta, options SnapshotSelectionOptions) bool {
	horizon := meta.GCHorizon
	if horizon == 0 {
		if options.RequireKnownIncrementalHorizon {
			return false
		}
		horizon = options.LiveGCHorizon
	}
	return horizon == 0 || meta.BaseIndex > horizon
}
