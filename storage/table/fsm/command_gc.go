// Copyright JAMF Software, LLC

package fsm

import (
	"encoding/binary"

	"github.com/armadakv/armada/regattapb"
)

type commandGC struct {
	*regattapb.Command
}

func (c commandGC) handle(ctx *updateContext) (UpdateResult, *regattapb.CommandResult, error) {
	horizon := ctx.seqno()
	val := make([]byte, 8)
	binary.LittleEndian.PutUint64(val, horizon)
	if err := ctx.batch.Set(sysGCHorizon, val, nil); err != nil {
		return ResultFailure, nil, err
	}
	return ResultGC, &regattapb.CommandResult{Revision: horizon}, nil
}
