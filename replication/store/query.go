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

// SelectBestSnapshot picks the best snapshot for followerIndex from metas.
// It returns false when no applicable snapshot exists.
func SelectBestSnapshot(metas []Meta, followerIndex uint64) (Meta, bool) {
	var selected Meta
	found := false
	for _, m := range metas {
		if m.TipIndex <= followerIndex {
			continue
		}
		if m.BaseIndex > followerIndex {
			continue
		}
		if !found {
			selected = m
			found = true
			continue
		}
		if m.BaseIndex > selected.BaseIndex {
			selected = m
			continue
		}
		if m.BaseIndex == selected.BaseIndex && m.TipIndex > selected.TipIndex {
			selected = m
		}
	}
	return selected, found
}
