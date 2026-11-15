// Copyright JAMF Software, LLC

package armadapb

import "errors"

// Validate checks the comparison operator, target, and optional target value.
func (c *Compare) Validate() error {
	if c == nil {
		return errors.New("compare must be set")
	}
	switch c.Result {
	case Compare_EQUAL, Compare_NOT_EQUAL, Compare_GREATER, Compare_LESS:
	default:
		return errors.New("invalid compare result")
	}
	switch c.Target {
	case Compare_VALUE, Compare_CREATE, Compare_MOD:
	default:
		return errors.New("invalid compare target")
	}
	switch c.TargetUnion.(type) {
	case nil:
		return nil
	case *Compare_Value:
		if c.Target == Compare_VALUE {
			return nil
		}
	case *Compare_CreateRevision:
		if c.Target == Compare_CREATE {
			return nil
		}
	case *Compare_ModRevision:
		if c.Target == Compare_MOD {
			return nil
		}
	}
	return errors.New("compare target does not match target_union")
}

func (req *TxnRequest) IsReadonly() bool {
	for _, op := range req.Success {
		if _, ok := op.Request.(*RequestOp_RequestRange); !ok {
			return false
		}
	}

	for _, op := range req.Failure {
		if _, ok := op.Request.(*RequestOp_RequestRange); !ok {
			return false
		}
	}
	return true
}
