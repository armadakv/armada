// Copyright JAMF Software, LLC

package fsm

import "github.com/armadakv/armada/storage/table/key"

func isTombstone(k key.Key) bool {
	return k.Kind == key.KindTombstone
}
