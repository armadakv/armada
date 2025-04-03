// Copyright 2017-2020 Lei Ni (nilei81@gmail.com) and other contributors.
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

package vfs

import (
	"github.com/armadakv/armada/vfs/errorfs"
)

// ErrInjected is an error injected for testing purposes.
var ErrInjected = errorfs.ErrInjected

// Injector injects errors into FS.
type Injector = errorfs.Injector

// ErrorFS is a gvfs.FS implementation.
type ErrorFS = errorfs.FS

// InjectIndex implements Injector
type InjectIndex = errorfs.InjectIndex

// Op is an enum describing the type of FS operations.
type Op = errorfs.Op

// OpRead describes read operations
var OpRead = errorfs.OpFileRead

// OpWrite describes write operations
var OpWrite = errorfs.OpFileWrite

// OpSync describes the fsync operation
var OpSync = errorfs.OpFileSync

// OnIndex creates and returns an injector instance that returns an ErrInjected
// on the (n+1)-th invocation of its MaybeError function.
func OnIndex(index int32) *InjectIndex {
	return errorfs.OnIndex(index)
}

// Wrap wraps an existing IFS implementation with the specified injector.
func Wrap(fs IFS, inj Injector) *ErrorFS {
	return errorfs.Wrap(fs, inj)
}
