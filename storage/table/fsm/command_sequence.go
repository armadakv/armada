// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/armadapb"
)

type commandSequence struct {
	*armadapb.Command
}

func (c commandSequence) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	res := &armadapb.CommandResult{Revision: ctx.seqno()}
	for _, cmd := range c.Sequence {
		_, cmdRes, err := wrapCommand(cmd).handle(ctx)
		if err != nil {
			return ResultFailure, nil, err
		}
		res.Responses = append(res.Responses, cmdRes.Responses...)
	}
	return ResultSuccess, res, nil
}
