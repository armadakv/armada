// Copyright Armada Contributors

package fsm

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/armadakv/armada/armadapb"
	sm "github.com/armadakv/armada/raft/statemachine"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	pb "google.golang.org/protobuf/proto"
)

func randomKey(rnd *rand.Rand) []byte {
	return fmt.Appendf(nil, "k%02d", rnd.IntN(24))
}

func randomOp(rnd *rand.Rand) *armadapb.RequestOp {
	switch rnd.IntN(3) {
	case 0:
		start := rnd.IntN(20)
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{
			Key: fmt.Appendf(nil, "k%02d", start), RangeEnd: fmt.Appendf(nil, "k%02d", start+1+rnd.IntN(4)),
		}}}
	case 1:
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestDeleteRange{RequestDeleteRange: &armadapb.RequestOp_DeleteRange{Key: randomKey(rnd)}}}
	default:
		return &armadapb.RequestOp{Request: &armadapb.RequestOp_RequestPut{RequestPut: &armadapb.RequestOp_Put{
			Key: randomKey(rnd), Value: fmt.Appendf(nil, "v%d", rnd.IntN(1000)),
		}}}
	}
}

func randomCommand(rnd *rand.Rand) *armadapb.Command {
	table := []byte(testTable)
	switch rnd.IntN(6) {
	case 0:
		start := rnd.IntN(20)
		return &armadapb.Command{Table: table, Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: fmt.Appendf(nil, "k%02d", start)}, RangeEnd: fmt.Appendf(nil, "k%02d", start+1+rnd.IntN(6))}
	case 1:
		return &armadapb.Command{Table: table, Type: armadapb.Command_DELETE, Kv: &armadapb.KeyValue{Key: randomKey(rnd)}}
	case 2:
		var batch []*armadapb.KeyValue
		for range 1 + rnd.IntN(4) {
			batch = append(batch, &armadapb.KeyValue{Key: randomKey(rnd), Value: fmt.Appendf(nil, "b%d", rnd.IntN(1000))})
		}
		return &armadapb.Command{Table: table, Type: armadapb.Command_PUT_BATCH, Batch: batch}
	case 3:
		var ops []*armadapb.RequestOp
		for range 1 + rnd.IntN(5) {
			ops = append(ops, randomOp(rnd))
		}
		return &armadapb.Command{Table: table, Type: armadapb.Command_TXN, Txn: &armadapb.Txn{Success: ops}}
	default:
		return &armadapb.Command{Table: table, Type: armadapb.Command_PUT, Kv: &armadapb.KeyValue{Key: randomKey(rnd), Value: fmt.Appendf(nil, "v%d", rnd.IntN(1000))}}
	}
}

// randomHistory applies a random workload and returns every live event.
func randomHistory(t *testing.T, seed uint64, entries int) (*FSM, []*armadapb.Event, uint64) {
	t.Helper()
	rnd := rand.New(rand.NewPCG(seed, seed))
	p, obs := observedSM(t, vfs.NewMem())
	t.Cleanup(func() { _ = p.Close() })

	index := uint64(1)
	for i := 0; i < entries; {
		n := 1 + rnd.IntN(4)
		batch := make([]sm.Entry, 0, n)
		for range n {
			batch = append(batch, sm.Entry{Index: index, Cmd: mustMarshallProto(randomCommand(rnd))})
			index++
			i++
		}
		_, err := p.Update(batch)
		require.NoError(t, err)
		if rnd.IntN(10) == 0 {
			// Move data into SSTs so that block filters take part.
			require.NoError(t, p.pebble.Load().Flush())
		}
	}

	var events []*armadapb.Event
	for _, c := range obs.commits {
		if c.Captured {
			events = append(events, c.Events...)
		}
	}
	return p, events, index - 1
}

func scanAll(t *testing.T, p *FSM, req WatchScanRequest) []*armadapb.Event {
	t.Helper()
	var events []*armadapb.Event
	to := req.To
	for req.From <= to {
		res, err := p.Lookup(req)
		require.NoError(t, err)
		r := res.(*WatchScanResponse)
		require.False(t, r.Compacted)
		require.GreaterOrEqual(t, r.Through, req.From, "every scan makes progress")
		events = append(events, r.Events...)
		req.From = r.Through + 1
	}
	return events
}

func withoutPrev(events []*armadapb.Event) []*armadapb.Event {
	res := make([]*armadapb.Event, len(events))
	for i, ev := range events {
		res[i] = &armadapb.Event{Type: ev.Type, Kv: ev.Kv}
	}
	return res
}

func inRange(events []*armadapb.Event, k, end []byte) []*armadapb.Event {
	var res []*armadapb.Event
	for _, ev := range events {
		key := ev.Kv.Key
		if end == nil && bytes.Equal(key, k) || end != nil && bytes.Compare(key, k) >= 0 && bytes.Compare(key, end) < 0 {
			res = append(res, ev)
		}
	}
	return res
}

func requireSameEvents(t *testing.T, want, got []*armadapb.Event) {
	t.Helper()
	require.Len(t, got, len(want), "number of events")
	for i := range want {
		if !pb.Equal(want[i], got[i]) {
			t.Fatalf("event %d differs:\nlive: %v\nscan: %v", i, want[i], got[i])
		}
	}
}

func TestWatchScan_MatchesLiveEvents(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			p, live, last := randomHistory(t, seed, 400)
			require.NotEmpty(t, live)
			all := WatchScanRequest{Key: []byte{0}, RangeEnd: []byte{0}, From: 1, To: last, PrevKV: true}

			requireSameEvents(t, live, scanAll(t, p, all))

			windowed := all
			windowed.MaxBytes = 300
			requireSameEvents(t, live, scanAll(t, p, windowed))

			noPrev := all
			noPrev.PrevKV = false
			requireSameEvents(t, withoutPrev(live), scanAll(t, p, noPrev))

			ranged := WatchScanRequest{Key: []byte("k05"), RangeEnd: []byte("k12"), From: 1, To: last, PrevKV: true, MaxBytes: 200}
			requireSameEvents(t, inRange(live, ranged.Key, ranged.RangeEnd), scanAll(t, p, ranged))

			single := WatchScanRequest{Key: []byte("k07"), From: 1, To: last, PrevKV: true}
			requireSameEvents(t, inRange(live, single.Key, nil), scanAll(t, p, single))

			// A window in the middle of the history.
			mid := WatchScanRequest{Key: []byte{0}, RangeEnd: []byte{0}, From: last / 3, To: 2 * last / 3}
			var want []*armadapb.Event
			for _, ev := range live {
				if r := uint64(ev.Kv.ModRevision); r >= mid.From && r <= mid.To {
					want = append(want, ev)
				}
			}
			requireSameEvents(t, withoutPrev(want), scanAll(t, p, mid))
		})
	}
}

func TestWatchScan_Compacted(t *testing.T) {
	p, _ := observedSM(t, vfs.NewMem())
	defer p.Close()
	applyCommands(t, p, 1, putCmd("a", "1"), putCmd("a", "2"), putCmd("a", "3"))
	horizon := uint64(2)
	applyCommands(t, p, 4, &armadapb.Command{Type: armadapb.Command_GC, LeaderIndex: &horizon})

	res, err := p.Lookup(WatchScanRequest{Key: []byte("a"), From: 2, To: 3})
	require.NoError(t, err)
	r := res.(*WatchScanResponse)
	require.True(t, r.Compacted)
	require.Equal(t, uint64(2), r.Floor)

	res, err = p.Lookup(WatchScanRequest{Key: []byte("a"), From: 3, To: 3, PrevKV: true})
	require.NoError(t, err)
	r = res.(*WatchScanResponse)
	require.False(t, r.Compacted)
	require.Len(t, r.Events, 1)
	require.Equal(t, "3", string(r.Events[0].Kv.Value))
}

func TestWatchScan_EmptyWindow(t *testing.T) {
	p, _ := observedSM(t, vfs.NewMem())
	defer p.Close()
	applyCommands(t, p, 1, putCmd("a", "1"), putCmd("b", "1"))

	res, err := p.Lookup(WatchScanRequest{Key: []byte("c"), From: 1, To: 2})
	require.NoError(t, err)
	r := res.(*WatchScanResponse)
	require.Empty(t, r.Events)
	require.Equal(t, uint64(2), r.Through)

	res, err = p.Lookup(WatchScanRequest{Key: []byte("a"), From: 3, To: 2})
	require.NoError(t, err)
	require.Equal(t, uint64(2), res.(*WatchScanResponse).Through)
}
