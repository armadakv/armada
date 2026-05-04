// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"

	"github.com/armadakv/armada/armadapb"
	"github.com/cockroachdb/pebble/v2"
)

type commandTxn struct {
	*armadapb.Command
}

func (c commandTxn) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	succ, rop, err := handleTxn(ctx, c.Txn.Compare, c.Txn.Success, c.Txn.Failure)
	if err != nil {
		return ResultFailure, nil, err
	}
	result := ResultSuccess
	if !succ {
		result = ResultFailure
	}
	return result, &armadapb.CommandResult{Revision: ctx.seqno(), Responses: rop}, nil
}

// handleTxn handle transaction operation, returns if the operation succeeded (if success, or fail was applied) list or respective results and error.
func handleTxn(ctx *updateContext, compare []*armadapb.Compare, success, fail []*armadapb.RequestOp) (bool, []*armadapb.ResponseOp, error) {
	if err := ctx.EnsureIndexed(); err != nil {
		return false, nil, err
	}
	ok, err := txnCompare(ctx.batch, compare)
	if err != nil {
		return false, nil, err
	}
	if ok {
		res, err := handleTxnOps(ctx, success)
		return true, res, err
	}
	res, err := handleTxnOps(ctx, fail)
	return false, res, err
}

func handleTxnOps(ctx *updateContext, req []*armadapb.RequestOp) ([]*armadapb.ResponseOp, error) {
	var results []*armadapb.ResponseOp
	for _, op := range req {
		switch o := op.Request.(type) {
		case *armadapb.RequestOp_RequestRange:
			response, err := lookup(ctx.batch, o.RequestRange)
			if err != nil {
				return nil, err
			}
			results = append(results, wrapResponseOp(response))
		case *armadapb.RequestOp_RequestPut:
			response, err := handlePut(ctx, o.RequestPut)
			if err != nil {
				return nil, err
			}
			results = append(results, wrapResponseOp(response))
		case *armadapb.RequestOp_RequestDeleteRange:
			response, err := handleDelete(ctx, o.RequestDeleteRange)
			if err != nil {
				return nil, err
			}
			results = append(results, wrapResponseOp(response))
		}
	}
	return results, nil
}

func txnCompare(reader pebble.Reader, compare []*armadapb.Compare) (bool, error) {
	keyBuf := bufferPool.Get()
	defer bufferPool.Put(keyBuf)
	for _, cmp := range compare {
		var (
			res bool
			err error
		)
		if cmp.RangeEnd != nil {
			res, err = func() (bool, error) {
				opts, err := iterOptionsForBounds(cmp.Key, cmp.RangeEnd)
				if err != nil {
					return false, err
				}
				iter, err := reader.NewIter(opts)
				if err != nil {
					return false, err
				}
				defer func() {
					_ = iter.Close()
				}()
				if !iter.First() {
					return false, nil
				}
				for iter.First(); iter.Valid(); {
					if isTombstone(iter.Value()) {
						currentKey := make([]byte, len(iter.Key()))
						copy(currentKey, iter.Key())
						if !iterNextUserKey(iter, currentKey) {
							break
						}
						continue
					}
					if !txnCompareSingle(cmp, iter.Value()) {
						return false, nil
					}
					iter.Next()
				}
				return true, nil
			}()
		} else {
			res, err = func() (bool, error) {
				// Use SeekPrefixGE to find the latest MVCC version of the key.
				// Encoding with ^uint64(0) (MaxUint64, stored as all-zeros) ensures
				// SeekPrefixGE lands on the first (latest) version within the prefix.
				if err := encodeUserKey(keyBuf, cmp.Key, ^uint64(0)); err != nil {
					return false, err
				}
				iter, err := reader.NewIter(nil)
				if err != nil {
					return false, err
				}
				defer func() {
					_ = iter.Close()
				}()

				if !iter.SeekPrefixGE(keyBuf.Bytes()) {
					keyBuf.Reset()
					return false, nil
				}

				if isTombstone(iter.Value()) {
					keyBuf.Reset()
					return false, nil
				}

				value := iter.Value()
				if !txnCompareSingle(cmp, value) {
					keyBuf.Reset()
					return false, nil
				}

				keyBuf.Reset()
				return true, nil
			}()
		}
		if err != nil {
			return false, err
		}
		if !res {
			return false, nil
		}
	}
	return true, nil
}

func txnCompareSingle(cmp *armadapb.Compare, value []byte) bool {
	cmpValue := true
	if cmp.Target == armadapb.Compare_VALUE && cmp.TargetUnion != nil {
		switch cmp.Result {
		case armadapb.Compare_EQUAL:
			cmpValue = bytes.Equal(value, cmp.GetValue())
		case armadapb.Compare_NOT_EQUAL:
			cmpValue = !bytes.Equal(value, cmp.GetValue())
		case armadapb.Compare_GREATER:
			cmpValue = bytes.Compare(value, cmp.GetValue()) == 1
		case armadapb.Compare_LESS:
			cmpValue = bytes.Compare(value, cmp.GetValue()) == -1
		}
	}

	return cmpValue
}
