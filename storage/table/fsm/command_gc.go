// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/armadapb"
)

type commandGC struct {
	*armadapb.Command
}

func (c commandGC) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	horizon := ctx.seqno()
	if c.GcHorizon != nil {
		// Replication sets the leader index of every command it ships to the
		// index of the replicated entry, so a follower finds the horizon the
		// leader proposed only here.
		horizon = *c.GcHorizon
	}
	ctx.advanceGCHorizon(horizon)
	return ResultGC, &armadapb.CommandResult{Revision: ctx.gcHorizon}, nil
}
