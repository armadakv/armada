// Copyright JAMF Software, LLC

package fsm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/armadakv/armada/armadapb"
	rp "github.com/armadakv/armada/pebble"
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
	r.Equal(wrapResponseOp(&armadapb.ResponseOp_Put{PrevKv: &armadapb.KeyValue{Key: []byte("key_5"), Value: nil}}), res[0])

	// compare key_5 value with "value" and delete keys up to key_4 (non-inclusive)
	succ, res, err = handleTxn(c, []*armadapb.Compare{{Key: []byte("key_5"), TargetUnion: &armadapb.Compare_Value{Value: []byte("value")}}}, []*armadapb.RequestOp{{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{Key: []byte("key_1"), RangeEnd: []byte("key_4"), PrevKv: true}}}}, nil)
	r.True(succ)
	r.NoError(err)
	r.Len(res, 1)
	r.Equal(wrapResponseOp(&armadapb.ResponseOp_DeleteRange{
		Deleted: 3,
		PrevKvs: []*armadapb.KeyValue{
			{Key: []byte("key_1"), Value: []byte("value")},
			{Key: []byte("key_2"), Value: []byte("value")},
			{Key: []byte("key_3"), Value: []byte("value")},
		},
	}), res[0])

	r.NoError(c.Commit())

	iter, err := db.NewIter(allUserKeysOpts())
	r.NoError(err)
	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		// Skip MVCC tombstone entries — they are internal deletion markers and
		// are not visible to readers; only count live (non-tombstone) entries.
		if isTombstone(iter.Value()) {
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
			require.Equal(t, tt.want, txnCompareSingle(tt.args.cmp, tt.args.value))
		})
	}
}
