// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"cmp"

	"github.com/armadakv/armada/armadapb"
	"github.com/armadakv/armada/storage/table/key"
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
		if err := cmp.Validate(); err != nil {
			return false, err
		}
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
				found := false
				for iter.First(); iter.Valid(); {
					k, err := key.DecodeBytes(iter.Key())
					if err != nil {
						return false, err
					}
					if isTombstone(k) {
						currentKey := make([]byte, len(iter.Key()))
						copy(currentKey, iter.Key())
						if !iterNextUserKey(iter, currentKey) {
							break
						}
						continue
					}
					found = true
					matched, err := txnCompareEntry(cmp, k, iter)
					if err != nil {
						return false, err
					}
					if !matched {
						return false, nil
					}
					iterNextUserKey(iter, nil)
				}
				if err := iter.Error(); err != nil {
					return false, err
				}
				return found || txnCompareMissing(cmp), nil
			}()
		} else {
			res, err = func() (bool, error) {
				// Use SeekPrefixGE to find the latest MVCC version of the key.
				// Encoding with ^uint64(0) (MaxUint64, stored as all-zeros) ensures
				// SeekPrefixGE lands on the first (latest) version within the prefix.
				if err := encodeUserKeyPrefix(keyBuf, cmp.Key); err != nil {
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
					return txnCompareMissing(cmp), iter.Error()
				}

				k, err := key.DecodeBytes(iter.Key())
				if err != nil {
					return false, err
				}
				if isTombstone(k) {
					keyBuf.Reset()
					return txnCompareMissing(cmp), nil
				}

				keyBuf.Reset()
				return txnCompareEntry(cmp, k, iter)
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

func txnCompareMissing(c *armadapb.Compare) bool {
	return c.Target != armadapb.Compare_VALUE && txnCompareSingle(c, key.Key{}, nil)
}

func txnCompareEntry(c *armadapb.Compare, k key.Key, iter *pebble.Iterator) (bool, error) {
	if err := validateRevision(k.CreateSeqno); err != nil {
		return false, err
	}
	if err := validateRevision(k.Seqno); err != nil {
		return false, err
	}
	var value []byte
	if c.Target == armadapb.Compare_VALUE && c.TargetUnion != nil {
		var err error
		value, err = iter.ValueAndErr()
		if err != nil {
			return false, err
		}
	}
	return txnCompareSingle(c, k, value), nil
}

func txnCompareSingle(c *armadapb.Compare, k key.Key, value []byte) bool {
	var order int
	switch c.Target {
	case armadapb.Compare_VALUE:
		if c.TargetUnion == nil {
			// A VALUE predicate without a value retains Armada's existence check.
			return true
		}
		order = bytes.Compare(value, c.GetValue())
	case armadapb.Compare_CREATE:
		order = cmp.Compare(int64(k.CreateSeqno), c.GetCreateRevision())
	case armadapb.Compare_MOD:
		order = cmp.Compare(int64(k.Seqno), c.GetModRevision())
	default:
		return false
	}
	switch c.Result {
	case armadapb.Compare_EQUAL:
		return order == 0
	case armadapb.Compare_NOT_EQUAL:
		return order != 0
	case armadapb.Compare_GREATER:
		return order > 0
	case armadapb.Compare_LESS:
		return order < 0
	default:
		return false
	}
}
