// Copyright Armada Contributors

package main

import (
	"bytes"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/stretchr/testify/require"
)

func TestPrefixRangeEnd(t *testing.T) {
	tests := []struct {
		name   string
		prefix []byte
		want   []byte
	}{
		{name: "text", prefix: []byte("foo"), want: []byte("fop")},
		{name: "carry", prefix: []byte{0x12, 0xff}, want: []byte{0x13}},
		{name: "unbounded", prefix: []byte{0xff, 0xff}, want: []byte{0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, prefixRangeEnd(tt.prefix))
		})
	}
}

func TestBuildRangeRequest(t *testing.T) {
	t.Run("exact key", func(t *testing.T) {
		request, err := buildRangeRequest("users", []string{"alice"}, getOptions{linearizable: true})
		require.NoError(t, err)
		require.Equal(t, []byte("users"), request.GetTable())
		require.Equal(t, []byte("alice"), request.GetKey())
		require.Empty(t, request.GetRangeEnd())
		require.True(t, request.GetLinearizable())
	})

	t.Run("prefix", func(t *testing.T) {
		request, err := buildRangeRequest("users", []string{"user/"}, getOptions{prefix: true})
		require.NoError(t, err)
		require.Equal(t, []byte("user0"), request.GetRangeEnd())
	})

	t.Run("all streaming", func(t *testing.T) {
		request, err := buildRangeRequest("users", nil, getOptions{all: true, stream: true})
		require.NoError(t, err)
		require.Equal(t, []byte{0}, request.GetKey())
		require.Equal(t, []byte{0}, request.GetRangeEnd())
	})

	t.Run("missing table", func(t *testing.T) {
		_, err := buildRangeRequest("", []string{"alice"}, getOptions{})
		require.ErrorContains(t, err, "--table")
	})

	t.Run("conflicting output options", func(t *testing.T) {
		_, err := buildRangeRequest("users", []string{"alice"}, getOptions{keysOnly: true, valueOnly: true})
		require.ErrorContains(t, err, "--value-only")
	})

	t.Run("explicit end with prefix", func(t *testing.T) {
		_, err := buildRangeRequest("users", []string{"a", "z"}, getOptions{prefix: true})
		require.ErrorContains(t, err, "explicit range-end")
	})
}

func TestBuildDeleteRequest(t *testing.T) {
	request, err := buildDeleteRequest("users", []string{"user/"}, deleteOptions{prefix: true, prevKV: true, count: true})
	require.NoError(t, err)
	require.Equal(t, []byte("user0"), request.GetRangeEnd())
	require.True(t, request.GetPrevKv())
	require.True(t, request.GetCount())

	_, err = buildDeleteRequest("users", []string{"a", "z"}, deleteOptions{fromKey: true})
	require.ErrorContains(t, err, "explicit range-end")

	request, err = buildDeleteRequest("users", nil, deleteOptions{all: true})
	require.NoError(t, err)
	require.Equal(t, []byte{0}, request.GetKey())
	require.Equal(t, []byte{0}, request.GetRangeEnd())
}

func TestWriteRangeSimple(t *testing.T) {
	response := &armadapb.RangeResponse{
		Kvs: []*armadapb.KeyValue{
			{Key: []byte("one"), Value: []byte("first")},
			{Key: []byte("two"), Value: []byte("second")},
		},
		Count: 2,
	}

	t.Run("key values", func(t *testing.T) {
		var output bytes.Buffer
		require.NoError(t, writeRangeSimple(&output, response, false, false, false))
		require.Equal(t, "one\nfirst\ntwo\nsecond\n", output.String())
	})

	t.Run("values only", func(t *testing.T) {
		var output bytes.Buffer
		require.NoError(t, writeRangeSimple(&output, response, false, false, true))
		require.Equal(t, "first\nsecond\n", output.String())
	})

	t.Run("count", func(t *testing.T) {
		var output bytes.Buffer
		require.NoError(t, writeRangeSimple(&output, response, false, true, false))
		require.Equal(t, "2\n", output.String())
	})
}

func TestWriteJSONUsesProtobufJSON(t *testing.T) {
	var output bytes.Buffer
	require.NoError(t, writeJSON(&output, &armadapb.RangeResponse{Kvs: []*armadapb.KeyValue{{Key: []byte("key")}}}))
	require.JSONEq(t, `{"kvs":[{"key":"a2V5"}]}`, output.String())
}
