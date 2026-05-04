// Copyright JAMF Software, LLC

package fsm

import (
	"encoding/binary"

	"github.com/armadakv/armada/armadapb"
)

type commandGC struct {
	*armadapb.Command
}

func (c commandGC) handle(ctx *updateContext) (UpdateResult, *armadapb.CommandResult, error) {
	horizon := ctx.seqno()
	val := make([]byte, 8)
	binary.LittleEndian.PutUint64(val, horizon)
	if err := ctx.batch.Set(sysGCHorizon, val, nil); err != nil {
		return ResultFailure, nil, err
	}
	return ResultGC, &armadapb.CommandResult{Revision: horizon}, nil
}
