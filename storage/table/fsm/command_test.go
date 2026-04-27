// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"testing"

	rp "github.com/armadakv/armada/pebble"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/regattapb"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

func TestUpdateContext_Parse(t *testing.T) {
	type args struct {
		entry sm.Entry
	}
	type want struct {
		index uint64
		cmd   command
	}
	tests := []struct {
		name    string
		args    args
		want    want
		wantErr bool
	}{
		{
			name: "empty command",
			args: args{entry: sm.Entry{Cmd: nil}},
			want: want{index: 0, cmd: commandPut{}},
		},
		{
			name: "empty command with index",
			args: args{entry: sm.Entry{Index: 200}},
			want: want{index: 200, cmd: commandPut{}},
		},
		{
			name: "put command with index",
			args: args{entry: sm.Entry{Index: 200, Cmd: mustMarshallProto(&regattapb.Command{Type: regattapb.Command_PUT, Table: []byte("test"), Kv: &regattapb.KeyValue{Key: []byte("key")}})}},
			want: want{index: 200, cmd: commandPut{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			uc := updateContext{}
			cmd, err := parseCommand(&uc, tt.args.entry)
			if tt.wantErr {
				r.Error(err)
			}
			r.NoError(err)
			r.IsType(tt.want.cmd, cmd)
			r.Equal(tt.want.index, uc.index)
		})
	}
}

func TestUpdateContext_EnsureIndexed(t *testing.T) {
	r := require.New(t)
	db, err := rp.OpenDB("/", rp.WithFS(vfs.NewMem()))
	r.NoError(err)
	uc := updateContext{
		db:    db,
		batch: db.NewBatch(),
	}
	tk := []byte("key")
	tv := []byte("value")
	r.NoError(uc.batch.Set(tk, tv, nil))

	r.False(uc.batch.Indexed())

	r.NoError(uc.EnsureIndexed())
	r.True(uc.batch.Indexed())
	_, _, err = uc.batch.Get(tk)
	r.NoError(err)

	r.NoError(uc.EnsureIndexed())
	r.True(uc.batch.Indexed())
}

func TestUpdateContext_Commit(t *testing.T) {
	r := require.New(t)
	db, err := rp.OpenDB("/", rp.WithFS(vfs.NewMem()))
	r.NoError(err)

	uc := updateContext{
		db:    db,
		batch: db.NewBatch(),
		index: 150,
	}
	r.NoError(uc.Commit())

	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(uc.index, index)
}

// allKeysOpts returns *pebble.IterOptions for iterating over
// all the user keys.
func allUserKeysOpts() *pebble.IterOptions {
	return &pebble.IterOptions{
		LowerBound: mustEncodeKey(key.Key{
			KeyType: key.TypeUser,
			Key:     key.LatestMinKey,
		}),
		UpperBound: incrementRightmostByte(mustEncodeKey(key.Key{
			KeyType: key.TypeUser,
			Key:     key.LatestMaxKey,
		})),
	}
}

// liveUserKey holds a decoded live (non-tombstoned) user key and its value.
type liveUserKey struct {
	key   []byte
	value []byte
}

// liveUserKeys returns all logically live user keys from db, visiting only the
// latest MVCC version of each distinct user key and skipping any that are
// tombstoned (i.e. have been deleted via a tombstone write).
// It is the MVCC-aware analogue of iterating with allUserKeysOpts for tests
// that validate logical state rather than raw pebble layout.
func liveUserKeys(t *testing.T, db pebble.Reader) []liveUserKey {
	t.Helper()
	iter, err := db.NewIter(allUserKeysOpts())
	if err != nil {
		t.Fatalf("liveUserKeys: could not open iterator: %v", err)
	}
	defer func() { _ = iter.Close() }()

	var result []liveUserKey
	for iter.First(); iter.Valid(); {
		rawKey := make([]byte, len(iter.Key()))
		copy(rawKey, iter.Key())

		val := iter.Value()
		if isTombstone(val) {
			// This key is logically deleted — skip the entire user-key prefix.
			iterNextUserKey(iter, rawKey)
			continue
		}

		k, decErr := key.DecodeBytes(rawKey)
		if decErr != nil {
			t.Fatalf("liveUserKeys: could not decode key: %v", decErr)
		}
		userKey := make([]byte, len(k.Key))
		copy(userKey, k.Key)
		v := make([]byte, len(val))
		copy(v, val)
		result = append(result, liveUserKey{key: userKey, value: v})

		// Advance past all older MVCC versions of this user key.
		iterNextUserKey(iter, rawKey)
	}
	return result
}

// decodeKey into *key.Key as pointed by the supplied *pebble.Iterator.
func decodeKey(t *testing.T, iter *pebble.Iterator, k *key.Key) {
	dec := key.NewDecoder(bytes.NewReader(iter.Key()))
	if err := dec.Decode(k); err != nil {
		t.Fatalf("could not decode key: %v", err)
	}
}
