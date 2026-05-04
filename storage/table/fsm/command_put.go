// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/armadapb"
)

type commandPut struct {
	*armadapb.Command
}

func (c commandPut) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	resp, err := handlePut(ctx, &armadapb.RequestOp_Put{
		Key:    c.Kv.Key,
		Value:  c.Kv.Value,
		PrevKv: c.PrevKvs,
	})
	if err != nil {
		return ResultFailure, nil, err
	}
	return ResultSuccess, &armadapb.CommandResult{
		Revision:  ctx.seqno(),
		Responses: []*armadapb.ResponseOp{wrapResponseOp(resp)},
	}, nil
}

func handlePut(ctx *updateContext, put *armadapb.RequestOp_Put) (*armadapb.ResponseOp_Put, error) {
	resp := &armadapb.ResponseOp_Put{}
	keyBuf := bufferPool.Get()
	defer bufferPool.Put(keyBuf)
	if err := encodeUserKey(keyBuf, put.Key, ctx.seqno()); err != nil {
		return nil, err
	}
	if put.PrevKv {
		if err := ctx.EnsureIndexed(); err != nil {
			return nil, err
		}
		rng, err := singleLookup(ctx.batch, &armadapb.RequestOp_Range{Key: put.Key})
		if err != nil {
			return nil, err
		}
		if len(rng.Kvs) == 1 {
			resp.PrevKv = rng.Kvs[0]
		}
	}
	if err := ctx.batch.Set(keyBuf.Bytes(), put.Value, nil); err != nil {
		return nil, err
	}
	return resp, nil
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
