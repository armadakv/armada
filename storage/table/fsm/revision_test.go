// Copyright Armada Contributors

package fsm

import (
	"bytes"
	"math"
	"testing"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/util/iterx"
	"github.com/stretchr/testify/require"
)

type commandCollector struct {
	commands [][]byte
}

func (w *commandCollector) Write(command []byte) (int, error) {
	w.commands = append(w.commands, append([]byte(nil), command...))
	return len(command), nil
}

func TestCreationRevisionIncarnationsAndRangeFilters(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	entries := []sm.Entry{
		putEntry(10, []byte("a"), []byte("old-a")),
		putEntry(20, []byte("b"), []byte("old-b")),
		putEntry(30, []byte("a"), []byte("new-a")),
		{Index: 40, Cmd: mustMarshallProto(&armadapb.Command{Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("b")}})},
		putEntry(50, []byte("b"), []byte("new-b")),
		putEntry(60, []byte("c"), []byte("c")),
	}
	_, err := p.Update(entries)
	require.NoError(t, err)

	result, err := p.Lookup(&armadapb.RequestOp_Range{Key: []byte("a")})
	require.NoError(t, err)
	require.Equal(t, &armadapb.KeyValue{
		Key: []byte("a"), Value: []byte("new-a"), CreateRevision: 10, ModRevision: 30,
	}, result.(*armadapb.ResponseOp_Range).Kvs[0])

	result, err = p.Lookup(&armadapb.RequestOp_Range{
		Key: []byte("b"), MinCreateRevision: 1, MaxCreateRevision: 20,
	})
	require.NoError(t, err)
	require.Empty(t, result.(*armadapb.ResponseOp_Range).Kvs)

	result, err = p.Lookup(&armadapb.RequestOp_Range{
		Key: []byte("a"), RangeEnd: []byte("d"), MinCreateRevision: 30, Limit: 1,
	})
	require.NoError(t, err)
	response := result.(*armadapb.ResponseOp_Range)
	require.Equal(t, int64(1), response.Count)
	require.Equal(t, []byte("b"), response.Kvs[0].Key)
	require.Equal(t, int64(50), response.Kvs[0].CreateRevision)
	require.Equal(t, int64(50), response.Kvs[0].ModRevision)
	require.True(t, response.More)

	result, err = p.Lookup(&armadapb.RequestOp_Range{
		Key: []byte("a"), RangeEnd: []byte("d"), MinCreateRevision: 50, CountOnly: true,
	})
	require.NoError(t, err)
	response = result.(*armadapb.ResponseOp_Range)
	require.Equal(t, int64(2), response.Count)
	require.Empty(t, response.Kvs)

	result, err = p.Lookup(&armadapb.RequestOp_Range{
		Key: []byte("b"), KeysOnly: true, MinCreateRevision: 50,
	})
	require.NoError(t, err)
	kv := result.(*armadapb.ResponseOp_Range).Kvs[0]
	require.Equal(t, int64(50), kv.CreateRevision)
	require.Equal(t, int64(50), kv.ModRevision)
	require.Empty(t, kv.Value)
}

func TestModificationRevisionFilters(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	_, err := p.Update([]sm.Entry{
		putEntry(10, []byte("a"), []byte("old")),
		putEntry(20, []byte("b"), []byte("deleted")),
		putEntry(30, []byte("a"), []byte("updated")),
		{Index: 40, Cmd: mustMarshallProto(&armadapb.Command{
			Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("b")},
		})},
		putEntry(50, []byte("c"), []byte("c")),
		putEntry(60, []byte("d"), []byte("d")),
	})
	require.NoError(t, err)

	for _, test := range []struct {
		name string
		req  *armadapb.RequestOp_Range
		keys []string
		more bool
	}{
		{"inclusive exact", &armadapb.RequestOp_Range{Key: []byte("a"), MinModRevision: 30, MaxModRevision: 30}, []string{"a"}, false},
		{"no historical exact", &armadapb.RequestOp_Range{Key: []byte("a"), MaxModRevision: 10}, nil, false},
		{"no historical range", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MaxModRevision: 20}, nil, false},
		{"inclusive range", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MinModRevision: 30, MaxModRevision: 50}, []string{"a", "c"}, false},
		{"filter before limit", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MinModRevision: 50, Limit: 1}, []string{"c"}, true},
		{"filtered tail not more", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MaxModRevision: 30, Limit: 1}, []string{"a"}, false},
		{"combined filters", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MaxCreateRevision: 10, MinModRevision: 30}, []string{"a"}, false},
		{"combined rejection", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("z"), MinCreateRevision: 20, MaxModRevision: 30}, nil, false},
		{"exclusive key bound", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte("c"), MinModRevision: 30}, []string{"a"}, false},
		{"max revision", &armadapb.RequestOp_Range{Key: []byte("a"), RangeEnd: []byte{0}, MaxModRevision: math.MaxInt64}, []string{"a", "c", "d"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, mode := range []string{"values", "keys", "count"} {
				t.Run(mode, func(t *testing.T) {
					test.req.KeysOnly = mode == "keys"
					test.req.CountOnly = mode == "count"
					result, err := p.Lookup(test.req)
					require.NoError(t, err)
					response := result.(*armadapb.ResponseOp_Range)
					require.Equal(t, int64(len(test.keys)), response.Count)
					require.Equal(t, test.more, response.More)
					var keys []string
					for _, kv := range response.Kvs {
						keys = append(keys, string(kv.Key))
						if test.req.KeysOnly {
							require.Empty(t, kv.Value)
						}
					}
					if test.req.CountOnly {
						require.Empty(t, response.Kvs)
					} else {
						require.Equal(t, test.keys, keys)
					}
					stream, err := iteratorLookup(p.pebble.Load(), test.req)
					require.NoError(t, err)
					require.Equal(t, response, iterx.First(stream))
				})
			}
		})
	}

	for _, req := range []*armadapb.RequestOp_Range{
		{Key: []byte("a"), MinModRevision: -1},
		{Key: []byte("a"), MaxModRevision: -1},
		{Key: []byte("a"), MinModRevision: 4, MaxModRevision: 3},
	} {
		for _, end := range [][]byte{nil, []byte("z")} {
			req.RangeEnd = end
			_, err := p.Lookup(req)
			require.Error(t, err)
			_, err = iteratorLookup(p.pebble.Load(), req)
			require.Error(t, err)
		}
	}
}

func TestModificationRevisionTransactionReads(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	_, err := p.Update([]sm.Entry{putEntry(10, []byte("key"), []byte("old"))})
	require.NoError(t, err)

	sourceRevision := uint64(30)
	command := &armadapb.Command{
		Type: armadapb.Command_TXN, LeaderIndex: &sourceRevision,
		Txn: &armadapb.Txn{Success: []*armadapb.RequestOp{
			{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{
				Key: []byte("key"), Value: []byte("new"),
			}}},
			{Request: &armadapb.RequestOp_RequestRange{RequestRange: &armadapb.RequestOp_Range{
				Key: []byte("key"), MaxModRevision: 10,
			}}},
			{Request: &armadapb.RequestOp_RequestRange{RequestRange: &armadapb.RequestOp_Range{
				Key: []byte("key"), MinCreateRevision: 10, MaxCreateRevision: 10,
				MinModRevision: 30, MaxModRevision: 30,
			}}},
		}},
	}
	entries, err := p.Update([]sm.Entry{{Index: 100, Cmd: mustMarshallProto(command)}})
	require.NoError(t, err)
	var result armadapb.CommandResult
	require.NoError(t, result.UnmarshalVT(entries[0].Result.Data))
	require.Zero(t, result.Responses[1].GetResponseRange().Count)
	require.Equal(t, []*armadapb.KeyValue{{
		Key: []byte("key"), Value: []byte("new"), CreateRevision: 10, ModRevision: 30,
	}}, result.Responses[2].GetResponseRange().Kvs)

	response, err := p.Lookup(&armadapb.TxnRequest{Success: command.Txn.Success[1:]})
	require.NoError(t, err)
	txn := response.(*armadapb.TxnResponse)
	require.Zero(t, txn.Responses[0].GetResponseRange().Count)
	require.Equal(t, result.Responses[2].GetResponseRange(), txn.Responses[1].GetResponseRange())
}

func TestCreationRevisionSameSequenceReplacementAndRestoreMetadata(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	sequence := &armadapb.Command{
		Type: armadapb.Command_SEQUENCE,
		Sequence: []*armadapb.Command{
			{Type: armadapb.Command_PUT, Kv: &armadapb.KeyValue{Key: []byte("same"), Value: []byte("first")}},
			{Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("same")}},
			{Type: armadapb.Command_PUT, Kv: &armadapb.KeyValue{Key: []byte("same"), Value: []byte{0}}},
		},
	}
	_, err := p.Update([]sm.Entry{{Index: 70, Cmd: mustMarshallProto(sequence)}})
	require.NoError(t, err)
	require.Equal(t, 1, physicalVersionCount(p.pebble.Load(), []byte("same")))

	result, err := p.Lookup(&armadapb.RequestOp_Range{Key: []byte("same")})
	require.NoError(t, err)
	kv := result.(*armadapb.ResponseOp_Range).Kvs[0]
	require.Equal(t, []byte{0}, kv.Value)
	require.Equal(t, int64(70), kv.CreateRevision)
	require.Equal(t, int64(70), kv.ModRevision)

	staleRevision := uint64(30)
	stale := &armadapb.Command{
		Type:        armadapb.Command_PUT,
		LeaderIndex: &staleRevision,
		Kv:          &armadapb.KeyValue{Key: []byte("restore"), Value: []byte("stale")},
	}
	_, err = p.Update([]sm.Entry{{Index: 80, Cmd: mustMarshallProto(stale)}})
	require.NoError(t, err)
	sourceRevision := uint64(55)
	restored := &armadapb.Command{
		Type:        armadapb.Command_PUT,
		LeaderIndex: &sourceRevision,
		Kv: &armadapb.KeyValue{
			Key: []byte("restore"), Value: []byte("snapshot"),
			CreateRevision: 20, ModRevision: 55,
		},
	}
	_, err = p.Update([]sm.Entry{{Index: 100, Cmd: mustMarshallProto(restored)}})
	require.NoError(t, err)
	result, err = p.Lookup(&armadapb.RequestOp_Range{Key: []byte("restore")})
	require.NoError(t, err)
	kv = result.(*armadapb.ResponseOp_Range).Kvs[0]
	require.Equal(t, []byte("snapshot"), kv.Value)
	require.Equal(t, int64(20), kv.CreateRevision)
	require.Equal(t, int64(55), kv.ModRevision)
}

func TestCreationRevisionValidationAndOpaqueValues(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()

	_, err := p.Update([]sm.Entry{putEntry(1, []byte("empty"), nil)})
	require.NoError(t, err)
	_, err = p.Update([]sm.Entry{putEntry(2, []byte("zero"), []byte{0})})
	require.NoError(t, err)

	for _, req := range []*armadapb.RequestOp_Range{
		{Key: []byte("empty"), MinCreateRevision: -1},
		{Key: []byte("empty"), MaxCreateRevision: -1},
		{Key: []byte("empty"), MinCreateRevision: 4, MaxCreateRevision: 3},
	} {
		_, err := p.Lookup(req)
		require.Error(t, err)
	}

	result, err := p.Lookup(&armadapb.RequestOp_Range{Key: []byte("empty")})
	require.NoError(t, err)
	require.Empty(t, result.(*armadapb.ResponseOp_Range).Kvs[0].Value)
	result, err = p.Lookup(&armadapb.RequestOp_Range{Key: []byte("zero")})
	require.NoError(t, err)
	require.True(t, bytes.Equal([]byte{0}, result.(*armadapb.ResponseOp_Range).Kvs[0].Value))

	sourceRevision := uint64(3)
	invalidSnapshot := &armadapb.Command{
		Type:        armadapb.Command_PUT,
		LeaderIndex: &sourceRevision,
		Kv: &armadapb.KeyValue{
			Key: []byte("invalid"), CreateRevision: -1, ModRevision: 3,
		},
	}
	_, err = p.Update([]sm.Entry{{Index: 3, Cmd: mustMarshallProto(invalidSnapshot)}})
	require.Error(t, err)
}

func TestLogicalSnapshotsPreserveCreationAndModificationRevisions(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	_, err := p.Update([]sm.Entry{
		putEntry(10, []byte("key"), []byte("first")),
		putEntry(20, []byte("key"), []byte("updated")),
	})
	require.NoError(t, err)

	full := &commandCollector{}
	_, err = commandSnapshot(p.pebble.Load(), "table", full, make(chan struct{}))
	require.NoError(t, err)
	require.Len(t, full.commands, 1)
	var command armadapb.Command
	require.NoError(t, command.UnmarshalVT(full.commands[0]))
	require.Equal(t, armadapb.Command_PUT, command.Type)
	require.Equal(t, int64(10), command.Kv.CreateRevision)
	require.Equal(t, int64(20), command.Kv.ModRevision)

	incremental := &commandCollector{}
	_, err = commandIncrementalSnapshot(p.pebble.Load(), "table", 10, incremental, make(chan struct{}))
	require.NoError(t, err)
	require.Len(t, incremental.commands, 1)
	require.NoError(t, command.UnmarshalVT(incremental.commands[0]))
	require.Equal(t, int64(10), command.Kv.CreateRevision)
	require.Equal(t, int64(20), command.Kv.ModRevision)

	delete := &armadapb.Command{Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("key")}}
	_, err = p.Update([]sm.Entry{{Index: 30, Cmd: mustMarshallProto(delete)}})
	require.NoError(t, err)
	incremental = &commandCollector{}
	_, err = commandIncrementalSnapshot(p.pebble.Load(), "table", 20, incremental, make(chan struct{}))
	require.NoError(t, err)
	require.Len(t, incremental.commands, 1)
	require.NoError(t, command.UnmarshalVT(incremental.commands[0]))
	require.Equal(t, armadapb.Command_DELETE, command.Type)
	require.Equal(t, int64(10), command.Kv.CreateRevision)
	require.Equal(t, int64(30), command.Kv.ModRevision)
}
