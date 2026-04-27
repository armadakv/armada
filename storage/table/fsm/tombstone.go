// Copyright JAMF Software, LLC

package fsm

// tombstoneValue is the sentinel stored as the value of a deleted key version.
// It is a single 0x00 byte. Valid protobuf values always start with a non-zero
// field tag byte, so this is unambiguous.
var tombstoneValue = []byte{0x00}

// isTombstone reports whether val is the MVCC deletion tombstone sentinel.
func isTombstone(val []byte) bool {
	return len(val) == 1 && val[0] == 0x00
}
