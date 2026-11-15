// Copyright JAMF Software, LLC

package fsm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/armadakv/armada/armadapb"
	rp "github.com/armadakv/armada/pebble"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

type errorReader struct{}

func (e errorReader) Get(key []byte) (value []byte, closer io.Closer, err error) {
	return nil, nil, errors.New("error")
}

func (e errorReader) NewIter(o *pebble.IterOptions) (*pebble.Iterator, error) {
	return nil, errors.New("error")
}

func (e errorReader) NewIterWithContext(ctx context.Context, o *pebble.IterOptions) (*pebble.Iterator, error) {
	return nil, errors.New("error")
}

func (e errorReader) Close() error {
	return errors.New("error")
}

func Test_txnCompare(t *testing.T) {
	sm := filledSM()
	defer sm.Close()
	loadedPebble := sm.pebble.Load()

	type args struct {
		reader  pebble.Reader
		compare []*armadapb.Compare
	}
	tests := []struct {
		name    string
		args    args
		want    bool
		wantErr bool
	}{
		{
			name: "key exist",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key: []byte(fmt.Sprintf(testKeyFormat, 1)),
					},
				},
			},
			want: true,
		},
		{
			name: "key does not exist",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key: []byte("nonsense"),
					},
				},
			},
			want: false,
		},
		{
			name: "non empty range",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:      []byte(fmt.Sprintf(testKeyFormat, 1)),
						RangeEnd: []byte(fmt.Sprintf(testKeyFormat, 5)),
					},
				},
			},
			want: true,
		},
		{
			name: "empty range",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:      []byte("nonsense"),
						RangeEnd: []byte("nonsense2"),
					},
				},
			},
			want: false,
		},
		{
			name: "fail fast",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key: []byte("nonsense"),
					},
					{
						Key: []byte(fmt.Sprintf(testKeyFormat, 1)),
					},
				},
			},
			want: false,
		},
		{
			name: "fail late",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key: []byte(fmt.Sprintf(testKeyFormat, 1)),
					},
					{
						Key: []byte("nonsense"),
					},
				},
			},
			want: false,
		},
		{
			name: "value comparison",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:         []byte(fmt.Sprintf(testKeyFormat, 1)),
						TargetUnion: &armadapb.Compare_Value{Value: []byte(testValue)},
					},
				},
			},
			want: true,
		},
		{
			name: "range value comparison",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:         []byte(fmt.Sprintf(testKeyFormat, 1)),
						RangeEnd:    []byte(fmt.Sprintf(testKeyFormat, 10)),
						TargetUnion: &armadapb.Compare_Value{Value: []byte(testValue)},
					},
				},
			},
			want: true,
		},
		{
			name: "fail value comparison",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:         []byte(fmt.Sprintf(testKeyFormat, 1)),
						TargetUnion: &armadapb.Compare_Value{Value: []byte("nonsense")},
					},
				},
			},
			want: false,
		},
		{
			name: "fail range comparison",
			args: args{
				reader: loadedPebble,
				compare: []*armadapb.Compare{
					{
						Key:         []byte(fmt.Sprintf(testKeyFormat, 1)),
						RangeEnd:    []byte(fmt.Sprintf(testKeyFormat, 10)),
						TargetUnion: &armadapb.Compare_Value{Value: []byte("nonsense")},
					},
				},
			},
			want: false,
		},
		{
			name: "fail to get key",
			args: args{
				reader: errorReader{},
				compare: []*armadapb.Compare{
					{
						Key: []byte(fmt.Sprintf(testKeyFormat, 1)),
					},
				},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			got, err := txnCompare(tt.args.reader, tt.args.compare)
			if tt.wantErr {
				r.Error(err)
				return
			}
			r.NoError(err)
			r.Equal(tt.want, got)
		})
	}
}

func TestTxnRevisionComparisons(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	_, err := p.Update([]sm.Entry{
		putEntry(10, []byte("a"), []byte("old")),
		putEntry(20, []byte("b"), []byte("current")),
		putEntry(30, []byte("a"), []byte("current")),
		putEntry(40, []byte("c"), []byte("deleted")),
		{Index: 50, Cmd: mustMarshallProto(&armadapb.Command{
			Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("c")},
		})},
		putEntry(60, []byte("c"), []byte("recreated")),
		putEntry(70, []byte("d"), []byte("deleted")),
		{Index: 80, Cmd: mustMarshallProto(&armadapb.Command{
			Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: []byte("d")},
		})},
	})
	require.NoError(t, err)

	for _, target := range []armadapb.Compare_CompareTarget{armadapb.Compare_CREATE, armadapb.Compare_MOD} {
		t.Run(target.String(), func(t *testing.T) {
			revision := int64(10)
			if target == armadapb.Compare_MOD {
				revision = 30
			}
			for _, test := range []struct {
				name   string
				key    string
				end    []byte
				result armadapb.Compare_CompareResult
				rhs    int64
				want   bool
			}{
				{"equal", "a", nil, armadapb.Compare_EQUAL, revision, true},
				{"not equal", "a", nil, armadapb.Compare_NOT_EQUAL, revision, false},
				{"different", "a", nil, armadapb.Compare_NOT_EQUAL, revision + 1, true},
				{"unequal", "a", nil, armadapb.Compare_EQUAL, revision + 1, false},
				{"greater", "a", nil, armadapb.Compare_GREATER, revision - 1, true},
				{"greater is strict", "a", nil, armadapb.Compare_GREATER, revision, false},
				{"less", "a", nil, armadapb.Compare_LESS, revision + 1, true},
				{"less is strict", "a", nil, armadapb.Compare_LESS, revision, false},
				{"signed minimum", "a", nil, armadapb.Compare_GREATER, math.MinInt64, true},
				{"signed maximum", "a", nil, armadapb.Compare_LESS, math.MaxInt64, true},
				{"latest incarnation", "c", nil, armadapb.Compare_EQUAL, 60, true},
				{"not prior incarnation", "c", nil, armadapb.Compare_EQUAL, 40, false},
				{"missing", "missing", nil, armadapb.Compare_EQUAL, 0, true},
				{"missing not nonzero", "missing", nil, armadapb.Compare_GREATER, 0, false},
				{"deleted", "d", nil, armadapb.Compare_EQUAL, 0, true},
				{"empty range", "x", []byte("z"), armadapb.Compare_EQUAL, 0, true},
				{"deleted-only range", "d", []byte("e"), armadapb.Compare_EQUAL, 0, true},
				{"empty range signed comparison", "x", []byte("z"), armadapb.Compare_GREATER, -1, true},
				{"all current keys", "a", []byte("c"), armadapb.Compare_LESS, 31, true},
				{"not just first key", "a", []byte("c"), armadapb.Compare_EQUAL, revision, false},
				{"exclusive end", "a", []byte("b"), armadapb.Compare_EQUAL, revision, true},
				{"skip tombstones", "c", []byte{0}, armadapb.Compare_EQUAL, 60, true},
				{"whole keyspace", "\x00", []byte{0}, armadapb.Compare_GREATER, 0, true},
			} {
				t.Run(test.name, func(t *testing.T) {
					c := &armadapb.Compare{Key: []byte(test.key), RangeEnd: test.end, Target: target, Result: test.result}
					if target == armadapb.Compare_CREATE {
						c.TargetUnion = &armadapb.Compare_CreateRevision{CreateRevision: test.rhs}
					} else {
						c.TargetUnion = &armadapb.Compare_ModRevision{ModRevision: test.rhs}
					}
					got, err := txnCompare(p.pebble.Load(), []*armadapb.Compare{c})
					require.NoError(t, err)
					require.Equal(t, test.want, got)
				})
			}
			got, err := txnCompare(p.pebble.Load(), []*armadapb.Compare{{
				Key: []byte("missing"), Target: target,
			}})
			require.NoError(t, err)
			require.True(t, got, "an omitted revision operand defaults to zero")
		})
	}

	for _, test := range []struct {
		key  string
		end  string
		want bool
	}{
		{"a", "c", true},
		{"d", "e", false},
	} {
		got, err := txnCompare(p.pebble.Load(), []*armadapb.Compare{{
			Key: []byte(test.key), RangeEnd: []byte(test.end),
			TargetUnion: &armadapb.Compare_Value{Value: []byte("current")},
		}})
		require.NoError(t, err)
		require.Equal(t, test.want, got)
	}
	got, err := txnCompare(p.pebble.Load(), []*armadapb.Compare{{Key: []byte("d"), RangeEnd: []byte("e")}})
	require.NoError(t, err)
	require.False(t, got, "a tombstone-only range does not satisfy an existence check")
}

func TestTxnRevisionCompareAndSwap(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	sourceRevision := uint64(10)
	_, err := p.Update([]sm.Entry{{Index: 100, Cmd: mustMarshallProto(&armadapb.Command{
		Type: armadapb.Command_PUT, LeaderIndex: &sourceRevision,
		Kv: &armadapb.KeyValue{Key: []byte("key"), Value: []byte("original")},
	})}})
	require.NoError(t, err)

	compare := []*armadapb.Compare{
		{Key: []byte("key"), Target: armadapb.Compare_CREATE, TargetUnion: &armadapb.Compare_CreateRevision{CreateRevision: 10}},
		{Key: []byte("key"), Target: armadapb.Compare_MOD, TargetUnion: &armadapb.Compare_ModRevision{ModRevision: 10}},
	}
	read := &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestRange{
		RequestRange: &armadapb.RequestOp_Range{Key: []byte("key")},
	}}
	readonly := &armadapb.TxnRequest{Compare: compare, Success: []*armadapb.RequestOp{read}, Failure: []*armadapb.RequestOp{read}}
	response, err := p.Lookup(readonly)
	require.NoError(t, err)
	require.True(t, response.(*armadapb.TxnResponse).Succeeded)

	sourceRevision = 20
	command := &armadapb.Command{
		Type: armadapb.Command_TXN, LeaderIndex: &sourceRevision,
		Txn: &armadapb.Txn{
			Compare: compare,
			Success: []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestPut{
				RequestPut: &armadapb.RequestOp_Put{Key: []byte("key"), Value: []byte("updated")},
			}}, read},
			Failure: []*armadapb.RequestOp{read},
		},
	}
	entries, err := p.Update([]sm.Entry{{Index: 101, Cmd: mustMarshallProto(command)}})
	require.NoError(t, err)
	require.Equal(t, uint64(ResultSuccess), entries[0].Result.Value)
	var result armadapb.CommandResult
	require.NoError(t, result.UnmarshalVT(entries[0].Result.Data))
	require.Equal(t, []*armadapb.KeyValue{{
		Key: []byte("key"), Value: []byte("updated"), CreateRevision: 10, ModRevision: 20,
	}}, result.Responses[1].GetResponseRange().Kvs)

	response, err = p.Lookup(readonly)
	require.NoError(t, err)
	require.False(t, response.(*armadapb.TxnResponse).Succeeded)

	sourceRevision = 30
	entries, err = p.Update([]sm.Entry{{Index: 102, Cmd: mustMarshallProto(command)}})
	require.NoError(t, err)
	require.Equal(t, uint64(ResultFailure), entries[0].Result.Value)
	result = armadapb.CommandResult{}
	require.NoError(t, result.UnmarshalVT(entries[0].Result.Data))
	require.Len(t, result.Responses, 1)
	require.Equal(t, int64(20), result.Responses[0].GetResponseRange().Kvs[0].ModRevision)

	command.Txn.Compare = []*armadapb.Compare{{
		Key: []byte("new"), Target: armadapb.Compare_CREATE,
		TargetUnion: &armadapb.Compare_CreateRevision{CreateRevision: 0},
	}}
	command.Txn.Success = []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestPut{
		RequestPut: &armadapb.RequestOp_Put{Key: []byte("new"), Value: []byte("once")},
	}}}
	for i, want := range []UpdateResult{ResultSuccess, ResultFailure} {
		sourceRevision = uint64(40 + i)
		entries, err = p.Update([]sm.Entry{{Index: uint64(103 + i), Cmd: mustMarshallProto(command)}})
		require.NoError(t, err)
		require.Equal(t, uint64(want), entries[0].Result.Value)
	}
}

func TestHandleTxnDeleteAll(t *testing.T) {
	db, err := rp.OpenDB("/", rp.WithFS(vfs.NewMem()))
	require.NoError(t, err)
	defer db.Close()

	ctx := &updateContext{batch: db.NewBatch(), db: db, index: 1}
	defer func() { _ = ctx.Close() }()
	_, err = handlePutBatch(ctx, []*armadapb.RequestOp_Put{
		{Key: []byte("first"), Value: []byte("one")},
		{Key: []byte("second"), Value: []byte("two")},
	})
	require.NoError(t, err)
	require.NoError(t, ctx.Commit())

	ctx.batch = db.NewBatch()
	ctx.index = 2
	succeeded, responses, err := handleTxn(ctx, nil, []*armadapb.RequestOp{{
		Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{
			Key:      []byte{0},
			RangeEnd: []byte{0},
			Count:    true,
		}},
	}}, nil)
	require.NoError(t, err)
	require.True(t, succeeded)
	require.Len(t, responses, 1)
	require.Equal(t, int64(2), responses[0].GetResponseDeleteRange().GetDeleted())
	require.NoError(t, ctx.Commit())
	require.Empty(t, liveUserKeys(t, db))
}

func Test_handleTxn(t *testing.T) {
	r := require.New(t)

	db, err := rp.OpenDB("/", rp.WithFS(vfs.NewMem()))
	if err != nil {
		t.Fatalf("could not open pebble db: %v", err)
	}

	c := &updateContext{
		batch: db.NewBatch(),
		db:    db,
		index: 1,
	}
	defer func() { _ = c.Close() }()

	// Make the PUT_BATCH.
	_, err = handlePutBatch(c, []*armadapb.RequestOp_Put{
		{Key: []byte("key_1"), Value: []byte("value")},
		{Key: []byte("key_2"), Value: []byte("value")},
		{Key: []byte("key_3"), Value: []byte("value")},
		{Key: []byte("key_4"), Value: []byte("value")},
	})
	r.NoError(err)
	r.NoError(c.Commit())

	c.batch = db.NewBatch()

	// empty transaction
	succ, res, err := handleTxn(c, []*armadapb.Compare{{Key: []byte("key_1")}}, nil, nil)
	r.True(succ)
	r.NoError(err)
	r.Empty(res)

	// insert key_5 with nil value
	succ, res, err = handleTxn(c, []*armadapb.Compare{{Key: []byte("key_1")}}, []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{Key: []byte("key_5"), Value: nil}}}}, nil)
	r.True(succ)
	r.NoError(err)
	r.Len(res, 1)
	r.Equal(wrapResponseOp(&armadapb.ResponseOp_Put{}), res[0])

	// compare key_5 nil value and associate the key with "value"
	succ, res, err = handleTxn(c, []*armadapb.Compare{{Key: []byte("key_5"), TargetUnion: &armadapb.Compare_Value{Value: nil}}}, []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{Key: []byte("key_5"), Value: []byte("value"), PrevKv: true}}}}, nil)
	r.True(succ)
	r.NoError(err)
	r.Len(res, 1)
	r.Equal(wrapResponseOp(&armadapb.ResponseOp_Put{PrevKv: &armadapb.KeyValue{
		Key: []byte("key_5"), Value: nil, CreateRevision: 1, ModRevision: 1,
	}}), res[0])

	// compare key_5 value with "value" and delete keys up to key_4 (non-inclusive)
	succ, res, err = handleTxn(c, []*armadapb.Compare{{Key: []byte("key_5"), TargetUnion: &armadapb.Compare_Value{Value: []byte("value")}}}, []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{Key: []byte("key_1"), RangeEnd: []byte("key_4"), PrevKv: true}}}}, nil)
	r.True(succ)
	r.NoError(err)
	r.Len(res, 1)
	r.Equal(wrapResponseOp(&armadapb.ResponseOp_DeleteRange{
		Deleted: 3,
		PrevKvs: []*armadapb.KeyValue{
			{Key: []byte("key_1"), Value: []byte("value"), CreateRevision: 1, ModRevision: 1},
			{Key: []byte("key_2"), Value: []byte("value"), CreateRevision: 1, ModRevision: 1},
			{Key: []byte("key_3"), Value: []byte("value"), CreateRevision: 1, ModRevision: 1},
		},
	}), res[0])

	r.NoError(c.Commit())

	iter, err := db.NewIter(allUserKeysOpts())
	r.NoError(err)
	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		// Skip MVCC tombstone entries — they are internal deletion markers and
		// are not visible to readers; only count live (non-tombstone) entries.
		decoded, err := key.DecodeBytes(iter.Key())
		r.NoError(err)
		if isTombstone(decoded) {
			continue
		}
		count++
		r.Equal("value", string(iter.Value()))
	}
	// just keys key_4 and key_5 should remain as live entries
	r.Equal(2, count)

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}

func Test_txnCompareSingle(t *testing.T) {
	type args struct {
		cmp   *armadapb.Compare
		value []byte
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "empty compare",
			args: args{
				cmp:   &armadapb.Compare{},
				value: nil,
			},
			want: true,
		},
		{
			name: "EQUAL - equal value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_EQUAL,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("test")},
				},
				value: []byte("test"),
			},
			want: true,
		},
		{
			name: "EQUAL - unequal value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_EQUAL,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("test")},
				},
				value: []byte("testssadasd"),
			},
			want: false,
		},
		{
			name: "NOT EQUAL - equal value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_NOT_EQUAL,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("test")},
				},
				value: []byte("test"),
			},
			want: false,
		},
		{
			name: "NOT EQUAL - unequal value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_NOT_EQUAL,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("test")},
				},
				value: []byte("testytest"),
			},
			want: true,
		},
		{
			name: "GREATER - greater value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_GREATER,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("testa")},
				},
				value: []byte("testaa"),
			},
			want: true,
		},
		{
			name: "GREATER - lesser value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_GREATER,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("testa")},
				},
				value: []byte("test"),
			},
			want: false,
		},
		{
			name: "LESS - greater value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_LESS,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("test")},
				},
				value: []byte("testa"),
			},
			want: false,
		},
		{
			name: "LESS - lesser value",
			args: args{
				cmp: &armadapb.Compare{
					Result:      armadapb.Compare_LESS,
					TargetUnion: &armadapb.Compare_Value{Value: []byte("testa")},
				},
				value: []byte("test"),
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, txnCompareSingle(tt.args.cmp, key.Key{}, tt.args.value))
		})
	}
}
