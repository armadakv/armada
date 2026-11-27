// Copyright Armada Contributors

package armadaserver

import (
	"github.com/armadakv/armada/armadapb"
)

// WatchServer implements the Watch service from proto/armada.proto.
type WatchServer struct {
	armadapb.UnimplementedWatchServer
}
