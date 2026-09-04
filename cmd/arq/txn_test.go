// Copyright Armada Contributors

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/armadakv/armada/armadapb"
	"github.com/stretchr/testify/require"
)

func TestParseTxn(t *testing.T) {
	input := `
# only update pending records
cmp
value("status key") = "pending value"
value("owner") != "nobody"
then
put "status key" ready
get "status key"
else
delete "status key"
`
	request, err := parseTxn(strings.NewReader(input), "users")
	require.NoError(t, err)
	require.Equal(t, []byte("users"), request.GetTable())
	require.Len(t, request.GetCompare(), 2)
	require.Equal(t, []byte("status key"), request.GetCompare()[0].GetKey())
	require.Equal(t, []byte("pending value"), request.GetCompare()[0].GetValue())
	require.Equal(t, armadapb.Compare_NOT_EQUAL, request.GetCompare()[1].GetResult())
	require.Len(t, request.GetSuccess(), 2)
	require.Equal(t, []byte("ready"), request.GetSuccess()[0].GetRequestPut().GetValue())
	require.Equal(t, []byte("status key"), request.GetSuccess()[1].GetRequestRange().GetKey())
	require.Len(t, request.GetFailure(), 1)
	require.Equal(t, []byte("status key"), request.GetFailure()[0].GetRequestDeleteRange().GetKey())
	require.True(t, request.GetFailure()[0].GetRequestDeleteRange().GetCount())
}

func TestParseTxnAllowsEmptyCompareAndFailure(t *testing.T) {
	request, err := parseTxn(strings.NewReader("cmp\nthen\nput key value\nelse\n"), "table")
	require.NoError(t, err)
	require.Empty(t, request.GetCompare())
	require.Len(t, request.GetSuccess(), 1)
	require.Empty(t, request.GetFailure())
}

func TestParseTxnErrors(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "missing cmp", input: "then\nget key\n", want: "then section is out of order"},
		{name: "missing then", input: "cmp\nvalue(key) = value\n", want: "missing then"},
		{name: "bad comparison", input: "cmp\nkey = value\nthen\n", want: "invalid comparison"},
		{name: "bad operation", input: "cmp\nthen\nwatch key\n", want: "unsupported operation"},
		{name: "unterminated quote", input: "cmp\nthen\nput key \"value\n", want: "unterminated quote"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTxn(strings.NewReader(tt.input), "table")
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestWriteTxnSimple(t *testing.T) {
	response := &armadapb.TxnResponse{
		Succeeded: true,
		Responses: []*armadapb.ResponseOp{
			{Response: &armadapb.ResponseOp_ResponseRange{ResponseRange: &armadapb.ResponseOp_Range{Kvs: []*armadapb.KeyValue{{Key: []byte("key"), Value: []byte("value")}}}}},
			{Response: &armadapb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &armadapb.ResponseOp_DeleteRange{Deleted: 2}}},
		},
	}
	var output bytes.Buffer
	require.NoError(t, writeTxnSimple(&output, response))
	require.Equal(t, "SUCCESS\nkey\nvalue\n2\n", output.String())
}
