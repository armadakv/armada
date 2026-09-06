// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/armadapb"
)

type commandSequence struct {
	*armadapb.Command
}

func (c commandSequence) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	sequenceLeaderIndex := ctx.leaderIndex
	defer func() {
		// The outer sequence index is the durable progress watermark and revision
		// reported for the proposal. Child commands may use distinct source indexes.
		ctx.leaderIndex = sequenceLeaderIndex
	}()

	res := &armadapb.CommandResult{Revision: ctx.seqno()}
	for _, cmd := range c.Sequence {
		if cmd.LeaderIndex != nil {
			ctx.leaderIndex = cmd.LeaderIndex
		} else {
			// Nested sequences created on the source leader do not carry an index on
			// their children, so they inherit their enclosing command's source index.
			ctx.leaderIndex = sequenceLeaderIndex
		}
		_, cmdRes, err := wrapCommand(cmd).handle(ctx)
		if err != nil {
			return ResultFailure, nil, err
		}
		res.Responses = append(res.Responses, cmdRes.Responses...)
	}
	return ResultSuccess, res, nil
}
