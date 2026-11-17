// Copyright JAMF Software, LLC

package fsm

import (
	"fmt"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/key"
)

type commandDelete struct {
	*armadapb.Command
}

func (c commandDelete) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	resp, err := handleDeleteWithMetadata(ctx, &armadapb.RequestOp_DeleteRange{
		Key:      c.Kv.Key,
		RangeEnd: c.RangeEnd,
		PrevKv:   c.PrevKvs,
		Count:    c.Count,
	}, c.Kv.CreateRevision, c.Kv.ModRevision,
		c.Kv.CreateRevision != 0 || c.Kv.ModRevision != 0)
	if err != nil {
		return ResultFailure, nil, err
	}
	return ResultSuccess, &armadapb.CommandResult{
		Revision:  ctx.seqno(),
		Responses: []*armadapb.ResponseOp{wrapResponseOp(resp)},
	}, nil
}

func handleDelete(ctx *updateContext, del *armadapb.RequestOp_DeleteRange) (*armadapb.ResponseOp_DeleteRange, error) {
	return handleDeleteWithMetadata(ctx, del, 0, 0, false)
}

func handleDeleteWithMetadata(ctx *updateContext, del *armadapb.RequestOp_DeleteRange, sourceCreate, sourceMod int64, hasSourceMetadata bool) (*armadapb.ResponseOp_DeleteRange, error) {
	resp := &armadapb.ResponseOp_DeleteRange{}
	modSeqno := ctx.seqno()
	if hasSourceMetadata {
		if sourceCreate < 0 || sourceMod < 0 {
			return nil, fmt.Errorf("snapshot key revisions must be non-negative")
		}
		if sourceCreate > sourceMod {
			return nil, fmt.Errorf("snapshot creation revision exceeds modification revision")
		}
		modSeqno = uint64(sourceMod)
	}
	if err := validateRevision(modSeqno); err != nil {
		return nil, err
	}

	if del.RangeEnd != nil {
		if hasSourceMetadata {
			return nil, fmt.Errorf("snapshot metadata is only valid for a single-key delete")
		}
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
		type recordToDelete struct {
			userKey     []byte
			physicalKey []byte
			createSeqno uint64
		}
		var toTombstone []recordToDelete
		var prevKvs []*armadapb.KeyValue

		for iter.First(); iter.Valid(); {
			k, decErr := key.DecodeBytes(iter.Key())
			if decErr != nil {
				_ = iter.Close()
				return nil, decErr
			}

			if k.KeyType == key.TypeUser {
				if !isTombstone(k) {
					userKey := append([]byte(nil), k.Key...)
					physicalKey := append([]byte(nil), iter.Key()...)
					toTombstone = append(toTombstone, recordToDelete{
						userKey: userKey, physicalKey: physicalKey, createSeqno: k.CreateSeqno,
					})

					if del.PrevKv {
						val, err := iter.ValueAndErr()
						if err != nil {
							_ = iter.Close()
							return nil, err
						}
						prevKv, err := makeKeyValue(userKey, val, k.CreateSeqno, k.Seqno)
						if err != nil {
							_ = iter.Close()
							return nil, err
						}
						prevKvs = append(prevKvs, prevKv)
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
		for _, record := range toTombstone {
			keyBuf := bufferPool.Get()
			if err := encodeUserRecord(keyBuf, record.userKey, modSeqno, record.createSeqno, key.KindTombstone); err != nil {
				bufferPool.Put(keyBuf)
				return nil, err
			}
			if decoded, err := key.DecodeBytes(record.physicalKey); err != nil {
				bufferPool.Put(keyBuf)
				return nil, err
			} else if decoded.Seqno == modSeqno {
				if err := ctx.batch.Delete(record.physicalKey, nil); err != nil {
					bufferPool.Put(keyBuf)
					return nil, err
				}
			}
			if err := ctx.batch.Set(keyBuf.Bytes(), nil, nil); err != nil {
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
		if err := ctx.EnsureIndexed(); err != nil {
			return nil, err
		}
		current, value, physicalKey, found, err := currentRecord(ctx.batch, del.Key, del.PrevKv)
		if err != nil {
			return nil, err
		}
		if (!found || isTombstone(current)) && !hasSourceMetadata {
			return resp, nil
		}
		createSeqno := current.CreateSeqno
		if hasSourceMetadata {
			createSeqno = uint64(sourceCreate)
		}
		if err := validateRevision(createSeqno); err != nil {
			return nil, err
		}
		if del.Count || del.PrevKv {
			if found && !isTombstone(current) {
				resp.Deleted = 1
			}
		}
		if del.PrevKv && found && !isTombstone(current) {
			prevKv, err := makeKeyValue(del.Key, value, current.CreateSeqno, current.Seqno)
			if err != nil {
				return nil, err
			}
			resp.PrevKvs = []*armadapb.KeyValue{prevKv}
		}
		keyBuf := bufferPool.Get()
		defer bufferPool.Put(keyBuf)
		if err := encodeUserRecord(keyBuf, del.Key, modSeqno, createSeqno, key.KindTombstone); err != nil {
			return nil, err
		}
		if found && current.Seqno == modSeqno {
			if err := ctx.batch.Delete(physicalKey, nil); err != nil {
				return nil, err
			}
		}
		if err := ctx.batch.Set(keyBuf.Bytes(), nil, nil); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

type commandDeleteBatch struct {
	*armadapb.Command
}

func (c commandDeleteBatch) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	req := make([]*armadapb.RequestOp_DeleteRange, len(c.Batch))
	for i, kv := range c.Batch {
		req[i] = &armadapb.RequestOp_DeleteRange{
			Key: kv.Key,
		}
	}
	rop, err := handleDeleteBatch(ctx, req)
	if err != nil {
		return ResultFailure, nil, err
	}
	res := make([]*armadapb.ResponseOp, 0, len(c.Batch))
	for _, put := range rop {
		res = append(res, wrapResponseOp(put))
	}
	return ResultSuccess, &armadapb.CommandResult{
		Revision:  ctx.seqno(),
		Responses: res,
	}, nil
}

func handleDeleteBatch(ctx *updateContext, ops []*armadapb.RequestOp_DeleteRange) ([]*armadapb.ResponseOp_DeleteRange, error) {
	results := make([]*armadapb.ResponseOp_DeleteRange, len(ops))
	for i, op := range ops {
		res, err := handleDelete(ctx, op)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}
	return results, nil
}
