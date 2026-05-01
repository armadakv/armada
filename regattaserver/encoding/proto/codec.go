// Copyright JAMF Software, LLC

package proto

import (
	"errors"

	"google.golang.org/grpc/mem"

	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/proto" // Blank import to ensure proper replacement of the default codec.
)

// Name is the name registered for the proto compressor.
const Name = "proto"

var defaultBufferPool = mem.DefaultBufferPool()

func init() {
	encoding.RegisterCodecV2(&Codec{
		fallback: encoding.GetCodecV2("proto"),
	})
}

type vtprotoMessage interface {
	MarshalToSizedBufferVT(data []byte) (int, error)
	UnmarshalVT([]byte) error
	SizeVT() int
}

type Codec struct {
	fallback encoding.CodecV2
}

func (*Codec) Name() string { return Name }

func (c *Codec) Marshal(v any) (mem.BufferSlice, error) {
	if m, ok := v.(vtprotoMessage); ok {
		size := m.SizeVT()
		if mem.IsBelowBufferPoolingThreshold(size) {
			buf := make([]byte, size)
			if _, err := m.MarshalToSizedBufferVT(buf[:size]); err != nil {
				return nil, err
			}
			return mem.BufferSlice{mem.SliceBuffer(buf)}, nil
		}
		buf := defaultBufferPool.Get(size)
		if _, err := m.MarshalToSizedBufferVT((*buf)[:size]); err != nil {
			defaultBufferPool.Put(buf)
			return nil, err
		}
		return mem.BufferSlice{mem.NewBuffer(buf, defaultBufferPool)}, nil
	}

	if c.fallback == nil {
		return nil, errors.New("proto: unsupported message type")
	}
	return c.fallback.Marshal(v)
}

func (c *Codec) Unmarshal(data mem.BufferSlice, v any) error {
	if m, ok := v.(vtprotoMessage); ok {
		buf := data.MaterializeToBuffer(defaultBufferPool)
		defer buf.Free()
		return m.UnmarshalVT(buf.ReadOnlyData())
	}

	if c.fallback == nil {
		return errors.New("proto: unsupported message type")
	}
	return c.fallback.Unmarshal(data, v)
}
