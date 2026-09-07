// Copyright Armada Contributors

package fsm

import (
	"testing"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/armadakv/armada/storage/table/key"
	"github.com/stretchr/testify/require"
)

func TestCommandSequenceUsesEachChildLeaderIndex(t *testing.T) {
	r := require.New(t)
	p := emptySM()
	defer func() { r.NoError(p.Close()) }()

	firstLeaderIndex := uint64(10)
	finalLeaderIndex := uint64(20)
	updates, err := p.Update([]sm.Entry{{
		Index: 1,
		Cmd: mustMarshallProto(&armadapb.Command{
			Table:       []byte(testTable),
			Type:        armadapb.Command_SEQUENCE,
			LeaderIndex: &finalLeaderIndex,
			Sequence: []*armadapb.Command{
				{
					Table:       []byte(testTable),
					Type:        armadapb.Command_PUT,
					LeaderIndex: &firstLeaderIndex,
					Kv:          &armadapb.KeyValue{Key: []byte("key"), Value: []byte("first")},
				},
				{
					Table:       []byte(testTable),
					Type:        armadapb.Command_PUT,
					LeaderIndex: &finalLeaderIndex,
					Kv:          &armadapb.KeyValue{Key: []byte("key"), Value: []byte("second")},
				},
			},
		}),
	}})
	r.NoError(err)

	result := &armadapb.CommandResult{}
	r.NoError(result.UnmarshalVT(updates[0].Result.Data))
	r.Equal(finalLeaderIndex, result.Revision)

	var versions []uint64
	var values []string
	iter, err := p.pebble.Load().NewIter(allUserKeysOpts())
	r.NoError(err)
	defer func() { r.NoError(iter.Close()) }()
	for iter.First(); iter.Valid(); iter.Next() {
		physicalKey, err := key.DecodeBytes(iter.Key())
		r.NoError(err)
		r.Equal(key.TypeUser, physicalKey.KeyType)
		r.Equal([]byte("key"), physicalKey.Key)
		versions = append(versions, physicalKey.Seqno)
		values = append(values, string(iter.Value()))
	}
	r.NoError(iter.Error())
	r.Equal([]uint64{finalLeaderIndex, firstLeaderIndex}, versions)
	r.Equal([]string{"second", "first"}, values)

	storedLeaderIndex, err := readLocalIndex(p.pebble.Load(), sysLeaderIndex)
	r.NoError(err)
	r.Equal(finalLeaderIndex, storedLeaderIndex)
}
