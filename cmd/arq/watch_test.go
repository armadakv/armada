// Copyright Armada Contributors

package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/stretchr/testify/require"
)

func TestBuildWatchRequest(t *testing.T) {
	t.Run("exact key", func(t *testing.T) {
		req, err := buildWatchRequest("users", []string{"alice"}, watchOptions{rev: 7, prevKV: true})
		require.NoError(t, err)
		require.Equal(t, []byte("users"), req.GetTable())
		require.Equal(t, []byte("alice"), req.GetKey())
		require.Empty(t, req.GetRangeEnd())
		require.Equal(t, int64(7), req.GetStartRevision())
		require.True(t, req.GetPrevKv())
		require.True(t, req.GetFragment())
	})

	t.Run("prefix with filter", func(t *testing.T) {
		req, err := buildWatchRequest("users", []string{"user/"}, watchOptions{prefix: true, noDelete: true})
		require.NoError(t, err)
		require.Equal(t, []byte("user0"), req.GetRangeEnd())
		require.Equal(t, []armadapb.WatchCreateRequest_FilterType{armadapb.WatchCreateRequest_NODELETE}, req.GetFilters())
	})

	t.Run("all", func(t *testing.T) {
		req, err := buildWatchRequest("users", nil, watchOptions{all: true})
		require.NoError(t, err)
		require.Equal(t, []byte{0}, req.GetKey())
		require.Equal(t, []byte{0}, req.GetRangeEnd())
	})

	for name, tc := range map[string]struct {
		table string
		args  []string
		opts  watchOptions
		err   string
	}{
		"missing table":    {args: []string{"a"}, err: "--table"},
		"missing key":      {table: "t", err: "key must be provided"},
		"negative rev":     {table: "t", args: []string{"a"}, opts: watchOptions{rev: -1}, err: "--rev"},
		"every filter":     {table: "t", args: []string{"a"}, opts: watchOptions{noPut: true, noDelete: true}, err: "every event"},
		"two modes":        {table: "t", args: []string{"a"}, opts: watchOptions{prefix: true, fromKey: true}, err: "only one of"},
		"range with mode":  {table: "t", args: []string{"a", "b"}, opts: watchOptions{prefix: true}, err: "explicit range-end"},
		"all with key arg": {table: "t", args: []string{"a"}, opts: watchOptions{all: true}, err: "--all"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := buildWatchRequest(tc.table, tc.args, tc.opts)
			require.ErrorContains(t, err, tc.err)
		})
	}
}

type fakeWatchStream struct {
	responses []*armadapb.WatchResponse
}

func (s *fakeWatchStream) Recv() (*armadapb.WatchResponse, error) {
	if len(s.responses) == 0 {
		return nil, io.EOF
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

func TestRunWatch(t *testing.T) {
	k.Set("write-out", "simple")
	put := &armadapb.Event{Type: armadapb.Event_PUT, Kv: &armadapb.KeyValue{Key: []byte("a"), Value: []byte("2")}, PrevKv: &armadapb.KeyValue{Key: []byte("a"), Value: []byte("1")}}
	del := &armadapb.Event{Type: armadapb.Event_DELETE, Kv: &armadapb.KeyValue{Key: []byte("b")}}
	stream := &fakeWatchStream{responses: []*armadapb.WatchResponse{
		{Created: true, WatchId: 1},
		{WatchId: 1, Fragment: true, Events: []*armadapb.Event{put}},
		{WatchId: 1, Events: []*armadapb.Event{del}},
		{WatchId: 1},
	}}
	var out bytes.Buffer
	require.NoError(t, runWatch(stream, &out, true))
	require.Equal(t, "PUT\na\n1\na\n2\nDELETE\nb\n", out.String())

	canceled := &fakeWatchStream{responses: []*armadapb.WatchResponse{
		{WatchId: 1, Canceled: true, CancelReason: "compacted", CompactRevision: 42},
	}}
	err := runWatch(canceled, &out, false)
	require.ErrorContains(t, err, "compacted")
	require.ErrorContains(t, err, "first available revision 42")
}
