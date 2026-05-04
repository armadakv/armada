// Copyright JAMF Software, LLC

package fsm

import (
	"github.com/armadakv/armada/armadapb"
)

type commandDummy struct{}

func (c commandDummy) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	return ResultSuccess, &armadapb.CommandResult{Revision: ctx.seqno()}, nil
}
