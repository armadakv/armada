// Copyright Armada Contributors

package armadaserver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestWatchServer_Unimplemented(t *testing.T) {
	err := (&WatchServer{}).Watch(nil)
	require.Equal(t, codes.Unimplemented, status.Code(err))
}
