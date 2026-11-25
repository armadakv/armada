// Copyright JAMF Software, LLC

package fsm

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	sm "github.com/armadakv/armada/raft/statemachine"
	serrors "github.com/armadakv/armada/storage/errors"
	"github.com/stretchr/testify/require"
)

func TestFSM_Snapshot(t *testing.T) {
	type args struct {
		producingSMFactory func() *FSM
		receivingSMFactory func() *FSM
	}
	tests := []struct {
		name string
		args args
	}{
		{
			"recover using checkpoint (small DB)",
			args{
				producingSMFactory: filledSM,
				receivingSMFactory: emptySM,
			},
		},
		{
			"recover using checkpoint (large DB)",
			args{
				producingSMFactory: filledLargeValuesSM,
				receivingSMFactory: emptySM,
			},
		},
		{
			"recover using checkpoint (empty DB)",
			args{
				producingSMFactory: filledIndexOnlySM,
				receivingSMFactory: emptySM,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)

			p := tt.args.producingSMFactory()
			defer p.Close()

			want, err := p.GetHash()
			r.NoError(err)

			snp, err := p.PrepareSnapshot()
			r.NoError(err)

			ep := tt.args.receivingSMFactory()
			defer ep.Close()

			snapf, err := os.Create(filepath.Join(t.TempDir(), "snapshot-file"))
			if err == nil {
				defer snapf.Close()
			}
			r.NoError(err)

			t.Log("save snapshot started")
			err = p.SaveSnapshot(snp, snapf, nil)
			r.NoError(err)
			_, err = snapf.Seek(0, 0)
			r.NoError(err)

			t.Log("recover from snapshot started")
			stopc := make(chan struct{})
			err = ep.RecoverFromSnapshot(snapf, stopc)
			r.NoError(err)

			t.Log("recovery finished")

			got, err := ep.GetHash()
			r.NoError(err)
			r.Equal(want, got, "the hash of recovered DB should be the same as of the original one")

			l, err := p.Lookup(LocalIndexRequest{})
			r.NoError(err)
			el, err := ep.Lookup(LocalIndexRequest{})
			r.NoError(err)

			originIndex := l.(*IndexResponse).Index

			r.Equal(originIndex, el.(*IndexResponse).Index, "the applied indexes should be the same")

			r.NoError(ep.Close())
			idx, err := ep.Open(nil)
			r.NoError(err)
			r.Equal(originIndex, idx, "indexes after reopen should be the same")
		})
	}
}

func TestFSM_SnapshotStopped(t *testing.T) {
	type args struct {
		producingSMFactory func() *FSM
		receivingSMFactory func() *FSM
	}
	tests := []struct {
		name string
		args args
	}{
		{
			"stop recovery using checkpoint",
			args{
				producingSMFactory: filledLargeValuesSM,
				receivingSMFactory: emptySM,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			p := tt.args.producingSMFactory()
			defer p.Close()

			snp, err := p.PrepareSnapshot()
			r.NoError(err)

			ep := tt.args.receivingSMFactory()

			snapf, err := os.Create(filepath.Join(t.TempDir(), "snapshot-file"))
			r.NoError(err)
			t.Log("Save snapshot started")
			err = p.SaveSnapshot(snp, snapf, nil)
			r.NoError(err)
			_, err = snapf.Seek(0, 0)
			r.NoError(err)

			// stop on the first read: recoverers check stopc before each chunk
			stopc := make(chan struct{})
			defer func() {
				_ = snapf.Close()
				_ = ep.Close()
			}()
			t.Log("Recover from snapshot started")
			err = ep.RecoverFromSnapshot(&stopOnReadReader{r: snapf, stop: stopc}, stopc)
			r.ErrorIs(err, sm.ErrSnapshotStopped)

			t.Log("Recovery stopped")
		})
	}
}

// TestFSM_RecoverFromLegacySnapshot checks that a snapshot in the removed SST
// format is refused before the state machine is touched.
func TestFSM_RecoverFromLegacySnapshot(t *testing.T) {
	r := require.New(t)

	ep := filledSM()
	defer ep.Close()
	want, err := ep.GetHash()
	r.NoError(err)

	header := snapshotHeader{}
	header.setFormat(snapshotFormatLegacy)
	payload := append(header[:], []byte("legacy sst payload")...)

	err = ep.RecoverFromSnapshot(bytes.NewReader(payload), make(chan struct{}))
	r.ErrorIs(err, serrors.ErrUnsupportedSnapshotFormat)

	got, err := ep.GetHash()
	r.NoError(err)
	r.Equal(want, got, "a rejected snapshot must leave the DB untouched")
}

type stopOnReadReader struct {
	r    io.Reader
	stop chan struct{}
	once sync.Once
}

func (s *stopOnReadReader) Read(p []byte) (int, error) {
	s.once.Do(func() { close(s.stop) })
	return s.r.Read(p)
}
