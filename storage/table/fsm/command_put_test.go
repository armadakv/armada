// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"testing"

	"github.com/armadakv/armada/armadapb"
	rp "github.com/armadakv/armada/pebble"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

func TestCurrentRecordOptionalValue(t *testing.T) {
	p := emptySM()
	defer func() { require.NoError(t, p.Close()) }()
	userKey := []byte("key")
	value := bytes.Repeat([]byte("value"), 16*1024)
	_, err := p.Update([]sm.Entry{
		putEntry(10, userKey, []byte("old")),
		putEntry(20, userKey, value),
	})
	require.NoError(t, err)

	for _, withValue := range []bool{false, true} {
		record, gotValue, physicalKey, found, err := currentRecord(p.pebble.Load(), userKey, withValue)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, uint64(10), record.CreateSeqno)
		require.Equal(t, uint64(20), record.Seqno)
		decoded, err := key.DecodeBytes(physicalKey)
		require.NoError(t, err)
		require.Equal(t, userKey, decoded.Key)
		require.Equal(t, record.CreateSeqno, decoded.CreateSeqno)
		require.Equal(t, record.Seqno, decoded.Seqno)
		if withValue {
			require.Equal(t, value, gotValue)
		} else {
			require.Nil(t, gotValue)
		}
	}

	_, err = p.Update([]sm.Entry{{Index: 30, Cmd: mustMarshallProto(&armadapb.Command{
		Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: userKey},
	})}})
	require.NoError(t, err)
	record, gotValue, _, found, err := currentRecord(p.pebble.Load(), userKey, true)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, isTombstone(record))
	require.Nil(t, gotValue)
}

func Test_handlePut(t *testing.T) {
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

	// Make the PUT.
	req := &armadapb.RequestOp_Put{
		Key:    []byte("key_1"),
		Value:  []byte("value_1"),
		PrevKv: true,
	}
	res, err := handlePut(c, req)
	r.NoError(err)
	r.Equal(&armadapb.ResponseOp_Put{}, res)
	r.NoError(c.Commit())

	// Make the PUT update.
	req = &armadapb.RequestOp_Put{
		Key:    []byte("key_1"),
		Value:  []byte("value_2"),
		PrevKv: true,
	}

	c.batch = db.NewBatch()
	res, err = handlePut(c, req)
	r.NoError(err)
	r.Equal(&armadapb.ResponseOp_Put{PrevKv: &armadapb.KeyValue{
		Key: []byte("key_1"), Value: []byte("value_1"), CreateRevision: 1, ModRevision: 1,
	}}, res)
	r.NoError(c.Commit())

	iter, err := db.NewIter(allUserKeysOpts())
	r.NoError(err)
	iter.First()

	k := &key.Key{}
	decodeKey(t, iter, k)

	r.Equal(req.Key, k.Key)
	r.Equal(req.Value, iter.Value())

	// Assert that there are no more user keys.
	iter.Next()
	r.False(iter.Valid())
	r.NoError(iter.Close())

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}

func Test_handlePutBatch(t *testing.T) {
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
	ops := []*armadapb.RequestOp_Put{
		{Key: []byte("key_1"), Value: []byte("value")},
		{Key: []byte("key_2"), Value: []byte("value")},
		{Key: []byte("key_3"), Value: []byte("value")},
		{Key: []byte("key_4"), Value: []byte("value")},
	}
	_, err = handlePutBatch(c, ops)
	r.NoError(err)
	r.NoError(c.Commit())

	var (
		i int
		k = &key.Key{}
	)

	iter, err := db.NewIter(allUserKeysOpts())
	r.NoError(err)
	for iter.First(); iter.Valid(); iter.Next() {
		decodeKey(t, iter, k)

		r.Equal(ops[i].Key, k.Key)
		r.Equal(ops[i].Value, iter.Value())
		r.NoError(iter.Error())

		i++
	}
	r.Len(ops, i)
	r.NoError(iter.Close())

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}
