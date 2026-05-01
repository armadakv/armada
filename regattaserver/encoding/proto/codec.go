// Copyright JAMF Software, LLC

package proto

import (
	"errors"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

// Name is the name registered for the proto compressor.
const Name = "proto"

var defaultBufferPool = mem.DefaultBufferPool()

func init() {
	encoding.RegisterCodecV2(&Codec{})
}

type vtprotoMessage interface {
	MarshalToSizedBufferVT(data []byte) (int, error)
	UnmarshalVT([]byte) error
	SizeVT() int
}

// Codec is a gRPC CodecV2 implementation that marshals vtprotobuf messages
// using the fast vtproto path and falls back to the standard proto library
// for regular proto.Message types.
type Codec struct{}

func (*Codec) Name() string { return Name }

func (*Codec) Marshal(v any) (mem.BufferSlice, error) {
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

	if pm := protoMessageOf(v); pm != nil {
		size := proto.Size(pm)
		if mem.IsBelowBufferPoolingThreshold(size) {
			buf, err := proto.Marshal(pm)
			if err != nil {
				return nil, err
			}
			return mem.BufferSlice{mem.SliceBuffer(buf)}, nil
		}
		pool := defaultBufferPool
		buf := pool.Get(size)
		opts := proto.MarshalOptions{}
		if _, err := opts.MarshalAppend((*buf)[:0], pm); err != nil {
			pool.Put(buf)
			return nil, err
		}
		return mem.BufferSlice{mem.NewBuffer(buf, pool)}, nil
	}

	return nil, errors.New("proto: unsupported message type")
}

func (*Codec) Unmarshal(data mem.BufferSlice, v any) error {
	if m, ok := v.(vtprotoMessage); ok {
		buf := data.MaterializeToBuffer(defaultBufferPool)
		defer buf.Free()
		return m.UnmarshalVT(buf.ReadOnlyData())
	}

	if pm := protoMessageOf(v); pm != nil {
		buf := data.MaterializeToBuffer(defaultBufferPool)
		defer buf.Free()
		return proto.Unmarshal(buf.ReadOnlyData(), pm)
	}

	return errors.New("proto: unsupported message type")
}

func protoMessageOf(v any) proto.Message {
	switch v := v.(type) {
	case protoadapt.MessageV1:
		return protoadapt.MessageV2Of(v)
	case protoadapt.MessageV2:
		return v
	}
	return nil
}
