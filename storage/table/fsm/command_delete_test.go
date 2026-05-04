// Copyright JAMF Software, LLC

package fsm

import (
	"testing"

	rp "github.com/armadakv/armada/pebble"
	"github.com/armadakv/armada/armadapb"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
)

func Test_handleDelete(t *testing.T) {
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
	_, err = handlePut(c, &armadapb.RequestOp_Put{
		Key:   []byte("key_1"),
		Value: []byte("value_1"),
	})
	r.NoError(err)
	r.NoError(c.Commit())

	c.batch = db.NewBatch()

	// Make the DELETE.
	res, err := handleDelete(c, &armadapb.RequestOp_DeleteRange{
		Key:    []byte("key_1"),
		PrevKv: true,
	})
	r.NoError(err)
	r.Equal(&armadapb.ResponseOp_DeleteRange{Deleted: 1, PrevKvs: []*armadapb.KeyValue{{Key: []byte("key_1"), Value: []byte("value_1")}}}, res)
	r.NoError(c.Commit())

	// Assert that there are no more live user keys left.
	r.Empty(liveUserKeys(t, db))

	// Assert deleting non-existent key returns count 0.
	c.batch = db.NewBatch()
	res, err = handleDelete(c, &armadapb.RequestOp_DeleteRange{
		Key:   []byte("key_1"),
		Count: true,
	})
	r.NoError(err)
	r.Equal(&armadapb.ResponseOp_DeleteRange{Deleted: 0, PrevKvs: nil}, res)
	r.NoError(c.Commit())

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}

func Test_handleDeleteBatch(t *testing.T) {
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

	// Make the DELETE_BATCH.
	_, err = handleDeleteBatch(c, []*armadapb.RequestOp_DeleteRange{
		{Key: []byte("key_1")},
		{Key: []byte("key_2")},
		{Key: []byte("key_3")},
		{Key: []byte("key_4")},
	})
	r.NoError(err)
	r.NoError(c.Commit())

	// Assert that there are no more live user keys left.
	r.Empty(liveUserKeys(t, db))

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}

func Test_handleDeleteRange(t *testing.T) {
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

	// Make the DELETE RANGE - delete first two user keys.
	_, err = handleDelete(c, &armadapb.RequestOp_DeleteRange{Key: []byte("key_1"), RangeEnd: []byte("key_3")})
	r.NoError(err)
	r.NoError(c.Commit())

	// Assert that only key_3 and key_4 remain as live user keys.
	live := liveUserKeys(t, db)
	r.Len(live, 2)
	r.Equal([]byte("key_3"), live[0].key)
	r.Equal([]byte("key_4"), live[1].key)

	c.batch = db.NewBatch()

	// Make the DELETE RANGE - delete the rest of the user keys.
	_, err = handleDelete(c, &armadapb.RequestOp_DeleteRange{Key: []byte("key_1"), RangeEnd: wildcard})
	r.NoError(err)
	r.NoError(c.Commit())

	// Assert that there are no more live user keys left.
	r.Empty(liveUserKeys(t, db))

	// Check the system keys.
	index, err := readLocalIndex(db, sysLocalIndex)
	r.NoError(err)
	r.Equal(c.index, index)
}
