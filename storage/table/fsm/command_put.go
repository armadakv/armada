// Copyright JAMF Software, LLC

package fsm

import (
	"fmt"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
)

type commandPut struct {
	*armadapb.Command
}

func (c commandPut) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	resp, err := handlePutWithMetadata(ctx, &armadapb.RequestOp_Put{
		Key:    c.Kv.Key,
		Value:  c.Kv.Value,
		PrevKv: c.PrevKvs,
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

func handlePut(ctx *updateContext, put *armadapb.RequestOp_Put) (*armadapb.ResponseOp_Put, error) {
	return handlePutWithMetadata(ctx, put, 0, 0, false)
}

func handlePutWithMetadata(ctx *updateContext, put *armadapb.RequestOp_Put, sourceCreate, sourceMod int64, hasSourceMetadata bool) (*armadapb.ResponseOp_Put, error) {
	resp := &armadapb.ResponseOp_Put{}
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
	if err := ctx.EnsureIndexed(); err != nil {
		return nil, err
	}
	current, value, physicalKey, found, err := currentRecord(ctx.batch, put.Key, put.PrevKv)
	if err != nil {
		return nil, err
	}
	createSeqno := modSeqno
	if hasSourceMetadata {
		createSeqno = uint64(sourceCreate)
	} else if found && !isTombstone(current) {
		createSeqno = current.CreateSeqno
	}
	if err := validateRevision(createSeqno); err != nil {
		return nil, err
	}
	keyBuf := bufferPool.Get()
	defer bufferPool.Put(keyBuf)
	if err := encodeUserRecord(keyBuf, put.Key, modSeqno, createSeqno, key.KindValue); err != nil {
		return nil, err
	}
	if put.PrevKv {
		if found && !isTombstone(current) {
			kv, err := makeKeyValue(put.Key, value, current.CreateSeqno, current.Seqno)
			if err != nil {
				return nil, err
			}
			resp.PrevKv = kv
		}
	}
	if found && current.Seqno == modSeqno {
		if err := ctx.batch.Delete(physicalKey, nil); err != nil {
			return nil, err
		}
	}
	if err := ctx.batch.Set(keyBuf.Bytes(), put.Value, nil); err != nil {
		return nil, err
	}
	return resp, nil
}

func currentRecord(reader pebble.Reader, userKey []byte, withValue bool) (key.Key, []byte, []byte, bool, error) {
	keyBuf := bufferPool.Get()
	defer bufferPool.Put(keyBuf)
	if err := encodeUserKeyPrefix(keyBuf, userKey); err != nil {
		return key.Key{}, nil, nil, false, err
	}

	iter, err := reader.NewIter(nil)
	if err != nil {
		return key.Key{}, nil, nil, false, err
	}
	defer func() { _ = iter.Close() }()
	if !iter.SeekPrefixGE(keyBuf.Bytes()) {
		return key.Key{}, nil, nil, false, iter.Error()
	}
	decoded, err := key.DecodeBytes(iter.Key())
	if err != nil {
		return key.Key{}, nil, nil, false, err
	}
	var value []byte
	if withValue && !isTombstone(decoded) {
		v, err := iter.ValueAndErr()
		if err != nil {
			return key.Key{}, nil, nil, false, err
		}
		value = append([]byte(nil), v...)
	}
	physicalKey := append([]byte(nil), iter.Key()...)
	return decoded, value, physicalKey, true, nil
}

func makeKeyValue(userKey, value []byte, createSeqno, modSeqno uint64) (*armadapb.KeyValue, error) {
	if err := validateRevision(createSeqno); err != nil {
		return nil, err
	}
	if err := validateRevision(modSeqno); err != nil {
		return nil, err
	}
	return &armadapb.KeyValue{
		Key:            append([]byte(nil), userKey...),
		Value:          append([]byte(nil), value...),
		CreateRevision: int64(createSeqno),
		ModRevision:    int64(modSeqno),
	}, nil
}

type commandPutBatch struct {
	*armadapb.Command
}

func (c commandPutBatch) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	req := make([]*armadapb.RequestOp_Put, len(c.Batch))
	for i, kv := range c.Batch {
		req[i] = &armadapb.RequestOp_Put{
			Key:   kv.Key,
			Value: kv.Value,
		}
	}
	rop, err := handlePutBatch(ctx, req)
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

func handlePutBatch(ctx *updateContext, ops []*armadapb.RequestOp_Put) ([]*armadapb.ResponseOp_Put, error) {
	results := make([]*armadapb.ResponseOp_Put, len(ops))
	for i, op := range ops {
		res, err := handlePut(ctx, op)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}
	return results, nil
}
