// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/regattapb"
	"github.com/armadakv/armada/storage/table/key"
)

type commandDelete struct {
	*regattapb.Command
}

func (c commandDelete) handle(ctx *updateContext) (UpdateResult, *regattapb.CommandResult, error) {
	resp, err := handleDelete(ctx, &regattapb.RequestOp_DeleteRange{
		Key:      c.Kv.Key,
		RangeEnd: c.RangeEnd,
		PrevKv:   c.PrevKvs,
		Count:    c.Count,
	})
	if err != nil {
		return ResultFailure, nil, err
	}
	return ResultSuccess, &regattapb.CommandResult{
		Revision:  ctx.seqno(),
		Responses: []*regattapb.ResponseOp{wrapResponseOp(resp)},
	}, nil
}

func handleDelete(ctx *updateContext, del *regattapb.RequestOp_DeleteRange) (*regattapb.ResponseOp_DeleteRange, error) {
	resp := &regattapb.ResponseOp_DeleteRange{}

	if del.RangeEnd != nil {
		// Range delete: iterate the latest version of every distinct user key in
		// [del.Key, del.RangeEnd) and write a tombstone for each live key.
		//
		// We always need EnsureIndexed so we can read our own writes and correctly
		// skip keys that were already tombstoned earlier in the same batch.
		if err := ctx.EnsureIndexed(); err != nil {
			return nil, err
		}

		opts, err := iterOptionsForBounds(del.Key, del.RangeEnd)
		if err != nil {
			return nil, err
		}

		iter, err := ctx.batch.NewIter(opts)
		if err != nil {
			return nil, err
		}

		// Collect the live user keys in this range so we can write tombstones
		// after closing the iterator (pebble disallows mutating a batch while
		// an iterator over it is open).
		var toTombstone [][]byte
		var prevKvs []*regattapb.KeyValue

		for iter.First(); iter.Valid(); {
			k, decErr := key.DecodeBytes(iter.Key())
			if decErr != nil {
				_ = iter.Close()
				return nil, decErr
			}

			if k.KeyType == key.TypeUser {
				val := iter.Value()
				if !isTombstone(val) {
					userKey := make([]byte, len(k.Key))
					copy(userKey, k.Key)
					toTombstone = append(toTombstone, userKey)

					if del.PrevKv {
						v := make([]byte, len(val))
						copy(v, val)
						prevKvs = append(prevKvs, &regattapb.KeyValue{
							Key:   userKey,
							Value: v,
						})
					}
				}
			}

			currentKey := make([]byte, len(iter.Key()))
			copy(currentKey, iter.Key())
			if !iterNextUserKey(iter, currentKey) {
				break
			}
		}

		if err := iter.Close(); err != nil {
			return nil, err
		}

		// Write one tombstone per live user key at the current raft index.
		for _, userKey := range toTombstone {
			keyBuf := bufferPool.Get()
			if err := encodeUserKey(keyBuf, userKey, ctx.seqno()); err != nil {
				bufferPool.Put(keyBuf)
				return nil, err
			}
			if err := ctx.batch.Set(keyBuf.Bytes(), tombstoneValue, nil); err != nil {
				bufferPool.Put(keyBuf)
				return nil, err
			}
			bufferPool.Put(keyBuf)
		}

		// Only populate Deleted / PrevKvs when the caller explicitly asked for them.
		if del.Count || del.PrevKv {
			resp.Deleted = int64(len(toTombstone))
		}
		if del.PrevKv {
			resp.PrevKvs = prevKvs
		}
	} else {
		// Single-key delete: write one tombstone at the current raft index.
		//
		// When the caller requests prev-KV or a count we look up the current
		// version first so we can populate those fields and also avoid writing
		// a tombstone on top of an already-tombstoned (or absent) key.
		if del.PrevKv || del.Count {
			if err := ctx.EnsureIndexed(); err != nil {
				return nil, err
			}
			rng, err := singleLookup(ctx.batch, &regattapb.RequestOp_Range{
				Key:       del.Key,
				CountOnly: del.Count && !del.PrevKv,
			})
			if err != nil {
				return nil, err
			}
			// singleLookup already skips tombstones, so Count==0 means not found.
			if rng.Count == 0 {
				return resp, nil
			}
			resp.Deleted = rng.Count
			resp.PrevKvs = rng.Kvs
		} else {
			// No count/prevKV requested: still must not double-tombstone.
			// A cheap existence check avoids a pointless write.
			if err := ctx.EnsureIndexed(); err != nil {
				return nil, err
			}
			existing, err := singleLookup(ctx.batch, &regattapb.RequestOp_Range{
				Key:       del.Key,
				CountOnly: true,
			})
			if err != nil {
				return nil, err
			}
			if existing.Count == 0 {
				// Key does not exist or is already tombstoned; nothing to do.
				return resp, nil
			}
			// resp.Deleted intentionally left at 0 — caller did not ask for it.
		}

		keyBuf := bufferPool.Get()
		defer bufferPool.Put(keyBuf)
		if err := encodeUserKey(keyBuf, del.Key, ctx.seqno()); err != nil {
			return nil, err
		}
		if err := ctx.batch.Set(keyBuf.Bytes(), tombstoneValue, nil); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

type commandDeleteBatch struct {
	*regattapb.Command
}

func (c commandDeleteBatch) handle(ctx *updateContext) (UpdateResult, *regattapb.CommandResult, error) {
	req := make([]*regattapb.RequestOp_DeleteRange, len(c.Batch))
	for i, kv := range c.Batch {
		req[i] = &regattapb.RequestOp_DeleteRange{
			Key: kv.Key,
		}
	}
	rop, err := handleDeleteBatch(ctx, req)
	if err != nil {
		return ResultFailure, nil, err
	}
	res := make([]*regattapb.ResponseOp, 0, len(c.Batch))
	for _, put := range rop {
		res = append(res, wrapResponseOp(put))
	}
	return ResultSuccess, &regattapb.CommandResult{
		Revision:  ctx.seqno(),
		Responses: res,
	}, nil
}

func handleDeleteBatch(ctx *updateContext, ops []*regattapb.RequestOp_DeleteRange) ([]*regattapb.ResponseOp_DeleteRange, error) {
	results := make([]*regattapb.ResponseOp_DeleteRange, len(ops))
	for i, op := range ops {
		res, err := handleDelete(ctx, op)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}
	return results, nil
}
