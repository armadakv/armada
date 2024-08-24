// Copyright 2017-2021 Lei Ni (nilei81@gmail.com) and other contributors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package logdb implements the persistent log storage used by Dragonboat.

This package is internally used by Dragonboat, applications are not expected
to import this package.
*/
package logdb

import (
	"github.com/jamf/regatta/raft/logger"
	pb "github.com/jamf/regatta/raft/raftpb"
)

var (
	plog = logger.GetLogger("logdb")
)

// IReusableKey is the interface for keys that can be reused. A reusable key is
// usually obtained by calling the GetKey() function of the IContext
// instance.
type IReusableKey interface {
	// SetEntryKey sets the key to be an entry key for the specified Raft node
	// with the specified entry index.
	SetEntryKey(shardID uint64, replicaID uint64, index uint64)
	// SetStateKey sets the key to be an persistent state key suitable
	// for the specified Raft shard node.
	SetStateKey(shardID uint64, replicaID uint64)
	// SetMaxIndexKey sets the key to be the max possible index key for the
	// specified Raft shard node.
	SetMaxIndexKey(shardID uint64, replicaID uint64)
	// Key returns the underlying byte slice of the key.
	Key() []byte
	// Release releases the key instance so it can be reused in the future.
	Release()
}

// IContext is the per thread context used in the logdb module.
// IContext is expected to contain a list of reusable keys and byte
// slices that are owned per thread so they can be safely reused by the same
// thread when accessing ILogDB.
type IContext interface {
	// Destroy destroys the IContext instance.
	Destroy()
	// Reset resets the IContext instance, all previous returned keys and
	// buffers will be put back to the IContext instance and be ready to
	// be used for the next iteration.
	Reset()
	// GetKey returns a reusable key.
	GetKey() IReusableKey
	// GetValueBuffer returns a byte buffer with at least sz bytes in length.
	GetValueBuffer(sz uint64) []byte
	// GetWriteBatch returns a write batch or transaction instance.
	GetWriteBatch() interface{}
	// SetWriteBatch adds the write batch to the IContext instance.
	SetWriteBatch(wb interface{})
	// GetEntryBatch returns an entry batch instance.
	GetEntryBatch() pb.EntryBatch
	// GetLastEntryBatch returns an entry batch instance.
	GetLastEntryBatch() pb.EntryBatch
}
