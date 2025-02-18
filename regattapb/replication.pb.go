// Copyright JAMF Software, LLC

//
// Regatta replication protobuffer specification
//

// Code generated by protoc-gen-go. DO NOT EDIT.
// versions:
// 	protoc-gen-go v1.36.5
// 	protoc        v5.29.3
// source: replication.proto

package regattapb

import (
	protoreflect "google.golang.org/protobuf/reflect/protoreflect"
	protoimpl "google.golang.org/protobuf/runtime/protoimpl"
	reflect "reflect"
	sync "sync"
	unsafe "unsafe"
)

const (
	// Verify that this generated code is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(20 - protoimpl.MinVersion)
	// Verify that runtime/protoimpl is sufficiently up-to-date.
	_ = protoimpl.EnforceVersion(protoimpl.MaxVersion - 20)
)

type ReplicateError int32

const (
	// USE_SNAPSHOT occurs when leader has no longer the specified `leader_index` in the log.
	// Follower must use `GetSnapshot` to catch up.
	ReplicateError_USE_SNAPSHOT ReplicateError = 0
	// LEADER_BEHIND occurs when the index of the leader is smaller than requested `leader_index`.
	// This should never happen. Manual intervention needed.
	ReplicateError_LEADER_BEHIND ReplicateError = 1
)

// Enum value maps for ReplicateError.
var (
	ReplicateError_name = map[int32]string{
		0: "USE_SNAPSHOT",
		1: "LEADER_BEHIND",
	}
	ReplicateError_value = map[string]int32{
		"USE_SNAPSHOT":  0,
		"LEADER_BEHIND": 1,
	}
)

func (x ReplicateError) Enum() *ReplicateError {
	p := new(ReplicateError)
	*p = x
	return p
}

func (x ReplicateError) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (ReplicateError) Descriptor() protoreflect.EnumDescriptor {
	return file_replication_proto_enumTypes[0].Descriptor()
}

func (ReplicateError) Type() protoreflect.EnumType {
	return &file_replication_proto_enumTypes[0]
}

func (x ReplicateError) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use ReplicateError.Descriptor instead.
func (ReplicateError) EnumDescriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{0}
}

type Table_Type int32

const (
	Table_REPLICATED Table_Type = 0
	Table_LOCAL      Table_Type = 1
)

// Enum value maps for Table_Type.
var (
	Table_Type_name = map[int32]string{
		0: "REPLICATED",
		1: "LOCAL",
	}
	Table_Type_value = map[string]int32{
		"REPLICATED": 0,
		"LOCAL":      1,
	}
)

func (x Table_Type) Enum() *Table_Type {
	p := new(Table_Type)
	*p = x
	return p
}

func (x Table_Type) String() string {
	return protoimpl.X.EnumStringOf(x.Descriptor(), protoreflect.EnumNumber(x))
}

func (Table_Type) Descriptor() protoreflect.EnumDescriptor {
	return file_replication_proto_enumTypes[1].Descriptor()
}

func (Table_Type) Type() protoreflect.EnumType {
	return &file_replication_proto_enumTypes[1]
}

func (x Table_Type) Number() protoreflect.EnumNumber {
	return protoreflect.EnumNumber(x)
}

// Deprecated: Use Table_Type.Descriptor instead.
func (Table_Type) EnumDescriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{2, 0}
}

type MetadataRequest struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *MetadataRequest) Reset() {
	*x = MetadataRequest{}
	mi := &file_replication_proto_msgTypes[0]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *MetadataRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MetadataRequest) ProtoMessage() {}

func (x *MetadataRequest) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[0]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MetadataRequest.ProtoReflect.Descriptor instead.
func (*MetadataRequest) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{0}
}

type MetadataResponse struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Tables        []*Table               `protobuf:"bytes,1,rep,name=tables,proto3" json:"tables,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *MetadataResponse) Reset() {
	*x = MetadataResponse{}
	mi := &file_replication_proto_msgTypes[1]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *MetadataResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*MetadataResponse) ProtoMessage() {}

func (x *MetadataResponse) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[1]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use MetadataResponse.ProtoReflect.Descriptor instead.
func (*MetadataResponse) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{1}
}

func (x *MetadataResponse) GetTables() []*Table {
	if x != nil {
		return x.Tables
	}
	return nil
}

type Table struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Name          string                 `protobuf:"bytes,1,opt,name=name,proto3" json:"name,omitempty"`
	Type          Table_Type             `protobuf:"varint,2,opt,name=type,proto3,enum=replication.v1.Table_Type" json:"type,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *Table) Reset() {
	*x = Table{}
	mi := &file_replication_proto_msgTypes[2]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *Table) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*Table) ProtoMessage() {}

func (x *Table) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[2]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use Table.ProtoReflect.Descriptor instead.
func (*Table) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{2}
}

func (x *Table) GetName() string {
	if x != nil {
		return x.Name
	}
	return ""
}

func (x *Table) GetType() Table_Type {
	if x != nil {
		return x.Type
	}
	return Table_REPLICATED
}

type SnapshotRequest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// table is name of the table to stream
	Table         []byte `protobuf:"bytes,1,opt,name=table,proto3" json:"table,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *SnapshotRequest) Reset() {
	*x = SnapshotRequest{}
	mi := &file_replication_proto_msgTypes[3]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *SnapshotRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SnapshotRequest) ProtoMessage() {}

func (x *SnapshotRequest) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[3]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SnapshotRequest.ProtoReflect.Descriptor instead.
func (*SnapshotRequest) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{3}
}

func (x *SnapshotRequest) GetTable() []byte {
	if x != nil {
		return x.Table
	}
	return nil
}

type SnapshotChunk struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// data is chunk of snapshot
	Data []byte `protobuf:"bytes,1,opt,name=data,proto3" json:"data,omitempty"`
	// len is a length of data bytes
	Len uint64 `protobuf:"varint,2,opt,name=len,proto3" json:"len,omitempty"`
	// index the index for which the snapshot was created
	Index         uint64 `protobuf:"varint,3,opt,name=index,proto3" json:"index,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *SnapshotChunk) Reset() {
	*x = SnapshotChunk{}
	mi := &file_replication_proto_msgTypes[4]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *SnapshotChunk) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*SnapshotChunk) ProtoMessage() {}

func (x *SnapshotChunk) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[4]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use SnapshotChunk.ProtoReflect.Descriptor instead.
func (*SnapshotChunk) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{4}
}

func (x *SnapshotChunk) GetData() []byte {
	if x != nil {
		return x.Data
	}
	return nil
}

func (x *SnapshotChunk) GetLen() uint64 {
	if x != nil {
		return x.Len
	}
	return 0
}

func (x *SnapshotChunk) GetIndex() uint64 {
	if x != nil {
		return x.Index
	}
	return 0
}

// ReplicateRequest request of the replication data at given leader_index
type ReplicateRequest struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// table is name of the table to replicate
	Table []byte `protobuf:"bytes,1,opt,name=table,proto3" json:"table,omitempty"`
	// leader_index is the index in the leader raft log of the last stored item in the follower
	LeaderIndex   uint64 `protobuf:"varint,2,opt,name=leader_index,json=leaderIndex,proto3" json:"leader_index,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ReplicateRequest) Reset() {
	*x = ReplicateRequest{}
	mi := &file_replication_proto_msgTypes[5]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReplicateRequest) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReplicateRequest) ProtoMessage() {}

func (x *ReplicateRequest) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[5]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReplicateRequest.ProtoReflect.Descriptor instead.
func (*ReplicateRequest) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{5}
}

func (x *ReplicateRequest) GetTable() []byte {
	if x != nil {
		return x.Table
	}
	return nil
}

func (x *ReplicateRequest) GetLeaderIndex() uint64 {
	if x != nil {
		return x.LeaderIndex
	}
	return 0
}

// ReplicateResponse response to the ReplicateRequest
type ReplicateResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// Types that are valid to be assigned to Response:
	//
	//	*ReplicateResponse_CommandsResponse
	//	*ReplicateResponse_ErrorResponse
	Response isReplicateResponse_Response `protobuf_oneof:"response"`
	// leader_index is the largest applied leader index at the time of the client RPC.
	LeaderIndex   uint64 `protobuf:"varint,8,opt,name=leader_index,json=leaderIndex,proto3" json:"leader_index,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ReplicateResponse) Reset() {
	*x = ReplicateResponse{}
	mi := &file_replication_proto_msgTypes[6]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReplicateResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReplicateResponse) ProtoMessage() {}

func (x *ReplicateResponse) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[6]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReplicateResponse.ProtoReflect.Descriptor instead.
func (*ReplicateResponse) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{6}
}

func (x *ReplicateResponse) GetResponse() isReplicateResponse_Response {
	if x != nil {
		return x.Response
	}
	return nil
}

func (x *ReplicateResponse) GetCommandsResponse() *ReplicateCommandsResponse {
	if x != nil {
		if x, ok := x.Response.(*ReplicateResponse_CommandsResponse); ok {
			return x.CommandsResponse
		}
	}
	return nil
}

func (x *ReplicateResponse) GetErrorResponse() *ReplicateErrResponse {
	if x != nil {
		if x, ok := x.Response.(*ReplicateResponse_ErrorResponse); ok {
			return x.ErrorResponse
		}
	}
	return nil
}

func (x *ReplicateResponse) GetLeaderIndex() uint64 {
	if x != nil {
		return x.LeaderIndex
	}
	return 0
}

type isReplicateResponse_Response interface {
	isReplicateResponse_Response()
}

type ReplicateResponse_CommandsResponse struct {
	CommandsResponse *ReplicateCommandsResponse `protobuf:"bytes,1,opt,name=commands_response,json=commandsResponse,proto3,oneof"`
}

type ReplicateResponse_ErrorResponse struct {
	ErrorResponse *ReplicateErrResponse `protobuf:"bytes,2,opt,name=error_response,json=errorResponse,proto3,oneof"`
}

func (*ReplicateResponse_CommandsResponse) isReplicateResponse_Response() {}

func (*ReplicateResponse_ErrorResponse) isReplicateResponse_Response() {}

// ReplicateCommandsResponse sequence of replication commands
type ReplicateCommandsResponse struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// commands represent the
	Commands      []*ReplicateCommand `protobuf:"bytes,1,rep,name=commands,proto3" json:"commands,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ReplicateCommandsResponse) Reset() {
	*x = ReplicateCommandsResponse{}
	mi := &file_replication_proto_msgTypes[7]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReplicateCommandsResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReplicateCommandsResponse) ProtoMessage() {}

func (x *ReplicateCommandsResponse) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[7]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReplicateCommandsResponse.ProtoReflect.Descriptor instead.
func (*ReplicateCommandsResponse) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{7}
}

func (x *ReplicateCommandsResponse) GetCommands() []*ReplicateCommand {
	if x != nil {
		return x.Commands
	}
	return nil
}

type ReplicateCommand struct {
	state protoimpl.MessageState `protogen:"open.v1"`
	// leaderIndex represents the leader raft index of the given command
	LeaderIndex uint64 `protobuf:"varint,1,opt,name=leader_index,json=leaderIndex,proto3" json:"leader_index,omitempty"`
	// command holds the leader raft log command at leaderIndex
	Command       *Command `protobuf:"bytes,2,opt,name=command,proto3" json:"command,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ReplicateCommand) Reset() {
	*x = ReplicateCommand{}
	mi := &file_replication_proto_msgTypes[8]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReplicateCommand) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReplicateCommand) ProtoMessage() {}

func (x *ReplicateCommand) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[8]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReplicateCommand.ProtoReflect.Descriptor instead.
func (*ReplicateCommand) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{8}
}

func (x *ReplicateCommand) GetLeaderIndex() uint64 {
	if x != nil {
		return x.LeaderIndex
	}
	return 0
}

func (x *ReplicateCommand) GetCommand() *Command {
	if x != nil {
		return x.Command
	}
	return nil
}

type ReplicateErrResponse struct {
	state         protoimpl.MessageState `protogen:"open.v1"`
	Error         ReplicateError         `protobuf:"varint,1,opt,name=error,proto3,enum=replication.v1.ReplicateError" json:"error,omitempty"`
	unknownFields protoimpl.UnknownFields
	sizeCache     protoimpl.SizeCache
}

func (x *ReplicateErrResponse) Reset() {
	*x = ReplicateErrResponse{}
	mi := &file_replication_proto_msgTypes[9]
	ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
	ms.StoreMessageInfo(mi)
}

func (x *ReplicateErrResponse) String() string {
	return protoimpl.X.MessageStringOf(x)
}

func (*ReplicateErrResponse) ProtoMessage() {}

func (x *ReplicateErrResponse) ProtoReflect() protoreflect.Message {
	mi := &file_replication_proto_msgTypes[9]
	if x != nil {
		ms := protoimpl.X.MessageStateOf(protoimpl.Pointer(x))
		if ms.LoadMessageInfo() == nil {
			ms.StoreMessageInfo(mi)
		}
		return ms
	}
	return mi.MessageOf(x)
}

// Deprecated: Use ReplicateErrResponse.ProtoReflect.Descriptor instead.
func (*ReplicateErrResponse) Descriptor() ([]byte, []int) {
	return file_replication_proto_rawDescGZIP(), []int{9}
}

func (x *ReplicateErrResponse) GetError() ReplicateError {
	if x != nil {
		return x.Error
	}
	return ReplicateError_USE_SNAPSHOT
}

var File_replication_proto protoreflect.FileDescriptor

var file_replication_proto_rawDesc = string([]byte{
	0x0a, 0x11, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x70, 0x72,
	0x6f, 0x74, 0x6f, 0x12, 0x0e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e,
	0x2e, 0x76, 0x31, 0x1a, 0x0a, 0x6d, 0x76, 0x63, 0x63, 0x2e, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x22,
	0x11, 0x0a, 0x0f, 0x4d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x52, 0x65, 0x71, 0x75, 0x65,
	0x73, 0x74, 0x22, 0x41, 0x0a, 0x10, 0x4d, 0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x52, 0x65,
	0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x2d, 0x0a, 0x06, 0x74, 0x61, 0x62, 0x6c, 0x65, 0x73,
	0x18, 0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x15, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61,
	0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x54, 0x61, 0x62, 0x6c, 0x65, 0x52, 0x06, 0x74,
	0x61, 0x62, 0x6c, 0x65, 0x73, 0x22, 0x6e, 0x0a, 0x05, 0x54, 0x61, 0x62, 0x6c, 0x65, 0x12, 0x12,
	0x0a, 0x04, 0x6e, 0x61, 0x6d, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x09, 0x52, 0x04, 0x6e, 0x61,
	0x6d, 0x65, 0x12, 0x2e, 0x0a, 0x04, 0x74, 0x79, 0x70, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0e,
	0x32, 0x1a, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76,
	0x31, 0x2e, 0x54, 0x61, 0x62, 0x6c, 0x65, 0x2e, 0x54, 0x79, 0x70, 0x65, 0x52, 0x04, 0x74, 0x79,
	0x70, 0x65, 0x22, 0x21, 0x0a, 0x04, 0x54, 0x79, 0x70, 0x65, 0x12, 0x0e, 0x0a, 0x0a, 0x52, 0x45,
	0x50, 0x4c, 0x49, 0x43, 0x41, 0x54, 0x45, 0x44, 0x10, 0x00, 0x12, 0x09, 0x0a, 0x05, 0x4c, 0x4f,
	0x43, 0x41, 0x4c, 0x10, 0x01, 0x22, 0x27, 0x0a, 0x0f, 0x53, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f,
	0x74, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x12, 0x14, 0x0a, 0x05, 0x74, 0x61, 0x62, 0x6c,
	0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x05, 0x74, 0x61, 0x62, 0x6c, 0x65, 0x22, 0x4b,
	0x0a, 0x0d, 0x53, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74, 0x43, 0x68, 0x75, 0x6e, 0x6b, 0x12,
	0x12, 0x0a, 0x04, 0x64, 0x61, 0x74, 0x61, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x04, 0x64,
	0x61, 0x74, 0x61, 0x12, 0x10, 0x0a, 0x03, 0x6c, 0x65, 0x6e, 0x18, 0x02, 0x20, 0x01, 0x28, 0x04,
	0x52, 0x03, 0x6c, 0x65, 0x6e, 0x12, 0x14, 0x0a, 0x05, 0x69, 0x6e, 0x64, 0x65, 0x78, 0x18, 0x03,
	0x20, 0x01, 0x28, 0x04, 0x52, 0x05, 0x69, 0x6e, 0x64, 0x65, 0x78, 0x22, 0x4b, 0x0a, 0x10, 0x52,
	0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x12,
	0x14, 0x0a, 0x05, 0x74, 0x61, 0x62, 0x6c, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0c, 0x52, 0x05,
	0x74, 0x61, 0x62, 0x6c, 0x65, 0x12, 0x21, 0x0a, 0x0c, 0x6c, 0x65, 0x61, 0x64, 0x65, 0x72, 0x5f,
	0x69, 0x6e, 0x64, 0x65, 0x78, 0x18, 0x02, 0x20, 0x01, 0x28, 0x04, 0x52, 0x0b, 0x6c, 0x65, 0x61,
	0x64, 0x65, 0x72, 0x49, 0x6e, 0x64, 0x65, 0x78, 0x22, 0xeb, 0x01, 0x0a, 0x11, 0x52, 0x65, 0x70,
	0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x58,
	0x0a, 0x11, 0x63, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x73, 0x5f, 0x72, 0x65, 0x73, 0x70, 0x6f,
	0x6e, 0x73, 0x65, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x29, 0x2e, 0x72, 0x65, 0x70, 0x6c,
	0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x52, 0x65, 0x70, 0x6c, 0x69,
	0x63, 0x61, 0x74, 0x65, 0x43, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x73, 0x52, 0x65, 0x73, 0x70,
	0x6f, 0x6e, 0x73, 0x65, 0x48, 0x00, 0x52, 0x10, 0x63, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x73,
	0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x4d, 0x0a, 0x0e, 0x65, 0x72, 0x72, 0x6f,
	0x72, 0x5f, 0x72, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b,
	0x32, 0x24, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76,
	0x31, 0x2e, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x45, 0x72, 0x72, 0x52, 0x65,
	0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x48, 0x00, 0x52, 0x0d, 0x65, 0x72, 0x72, 0x6f, 0x72, 0x52,
	0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x21, 0x0a, 0x0c, 0x6c, 0x65, 0x61, 0x64, 0x65,
	0x72, 0x5f, 0x69, 0x6e, 0x64, 0x65, 0x78, 0x18, 0x08, 0x20, 0x01, 0x28, 0x04, 0x52, 0x0b, 0x6c,
	0x65, 0x61, 0x64, 0x65, 0x72, 0x49, 0x6e, 0x64, 0x65, 0x78, 0x42, 0x0a, 0x0a, 0x08, 0x72, 0x65,
	0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x22, 0x59, 0x0a, 0x19, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63,
	0x61, 0x74, 0x65, 0x43, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x73, 0x52, 0x65, 0x73, 0x70, 0x6f,
	0x6e, 0x73, 0x65, 0x12, 0x3c, 0x0a, 0x08, 0x63, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x73, 0x18,
	0x01, 0x20, 0x03, 0x28, 0x0b, 0x32, 0x20, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74,
	0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65,
	0x43, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x52, 0x08, 0x63, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64,
	0x73, 0x22, 0x61, 0x0a, 0x10, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x43, 0x6f,
	0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x12, 0x21, 0x0a, 0x0c, 0x6c, 0x65, 0x61, 0x64, 0x65, 0x72, 0x5f,
	0x69, 0x6e, 0x64, 0x65, 0x78, 0x18, 0x01, 0x20, 0x01, 0x28, 0x04, 0x52, 0x0b, 0x6c, 0x65, 0x61,
	0x64, 0x65, 0x72, 0x49, 0x6e, 0x64, 0x65, 0x78, 0x12, 0x2a, 0x0a, 0x07, 0x63, 0x6f, 0x6d, 0x6d,
	0x61, 0x6e, 0x64, 0x18, 0x02, 0x20, 0x01, 0x28, 0x0b, 0x32, 0x10, 0x2e, 0x6d, 0x76, 0x63, 0x63,
	0x2e, 0x76, 0x31, 0x2e, 0x43, 0x6f, 0x6d, 0x6d, 0x61, 0x6e, 0x64, 0x52, 0x07, 0x63, 0x6f, 0x6d,
	0x6d, 0x61, 0x6e, 0x64, 0x22, 0x4c, 0x0a, 0x14, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74,
	0x65, 0x45, 0x72, 0x72, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x12, 0x34, 0x0a, 0x05,
	0x65, 0x72, 0x72, 0x6f, 0x72, 0x18, 0x01, 0x20, 0x01, 0x28, 0x0e, 0x32, 0x1e, 0x2e, 0x72, 0x65,
	0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x52, 0x65, 0x70,
	0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x45, 0x72, 0x72, 0x6f, 0x72, 0x52, 0x05, 0x65, 0x72, 0x72,
	0x6f, 0x72, 0x2a, 0x35, 0x0a, 0x0e, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x45,
	0x72, 0x72, 0x6f, 0x72, 0x12, 0x10, 0x0a, 0x0c, 0x55, 0x53, 0x45, 0x5f, 0x53, 0x4e, 0x41, 0x50,
	0x53, 0x48, 0x4f, 0x54, 0x10, 0x00, 0x12, 0x11, 0x0a, 0x0d, 0x4c, 0x45, 0x41, 0x44, 0x45, 0x52,
	0x5f, 0x42, 0x45, 0x48, 0x49, 0x4e, 0x44, 0x10, 0x01, 0x32, 0x54, 0x0a, 0x08, 0x4d, 0x65, 0x74,
	0x61, 0x64, 0x61, 0x74, 0x61, 0x12, 0x48, 0x0a, 0x03, 0x47, 0x65, 0x74, 0x12, 0x1f, 0x2e, 0x72,
	0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x4d, 0x65,
	0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a, 0x20, 0x2e,
	0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x4d,
	0x65, 0x74, 0x61, 0x64, 0x61, 0x74, 0x61, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65, 0x32,
	0x56, 0x0a, 0x08, 0x53, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74, 0x12, 0x4a, 0x0a, 0x06, 0x53,
	0x74, 0x72, 0x65, 0x61, 0x6d, 0x12, 0x1f, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74,
	0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x53, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74, 0x52,
	0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a, 0x1d, 0x2e, 0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61,
	0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x53, 0x6e, 0x61, 0x70, 0x73, 0x68, 0x6f, 0x74,
	0x43, 0x68, 0x75, 0x6e, 0x6b, 0x30, 0x01, 0x32, 0x59, 0x0a, 0x03, 0x4c, 0x6f, 0x67, 0x12, 0x52,
	0x0a, 0x09, 0x52, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x12, 0x20, 0x2e, 0x72, 0x65,
	0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x52, 0x65, 0x70,
	0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x52, 0x65, 0x71, 0x75, 0x65, 0x73, 0x74, 0x1a, 0x21, 0x2e,
	0x72, 0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x69, 0x6f, 0x6e, 0x2e, 0x76, 0x31, 0x2e, 0x52,
	0x65, 0x70, 0x6c, 0x69, 0x63, 0x61, 0x74, 0x65, 0x52, 0x65, 0x73, 0x70, 0x6f, 0x6e, 0x73, 0x65,
	0x30, 0x01, 0x42, 0x0d, 0x5a, 0x0b, 0x2e, 0x2f, 0x72, 0x65, 0x67, 0x61, 0x74, 0x74, 0x61, 0x70,
	0x62, 0x62, 0x06, 0x70, 0x72, 0x6f, 0x74, 0x6f, 0x33,
})

var (
	file_replication_proto_rawDescOnce sync.Once
	file_replication_proto_rawDescData []byte
)

func file_replication_proto_rawDescGZIP() []byte {
	file_replication_proto_rawDescOnce.Do(func() {
		file_replication_proto_rawDescData = protoimpl.X.CompressGZIP(unsafe.Slice(unsafe.StringData(file_replication_proto_rawDesc), len(file_replication_proto_rawDesc)))
	})
	return file_replication_proto_rawDescData
}

var file_replication_proto_enumTypes = make([]protoimpl.EnumInfo, 2)
var file_replication_proto_msgTypes = make([]protoimpl.MessageInfo, 10)
var file_replication_proto_goTypes = []any{
	(ReplicateError)(0),               // 0: replication.v1.ReplicateError
	(Table_Type)(0),                   // 1: replication.v1.Table.Type
	(*MetadataRequest)(nil),           // 2: replication.v1.MetadataRequest
	(*MetadataResponse)(nil),          // 3: replication.v1.MetadataResponse
	(*Table)(nil),                     // 4: replication.v1.Table
	(*SnapshotRequest)(nil),           // 5: replication.v1.SnapshotRequest
	(*SnapshotChunk)(nil),             // 6: replication.v1.SnapshotChunk
	(*ReplicateRequest)(nil),          // 7: replication.v1.ReplicateRequest
	(*ReplicateResponse)(nil),         // 8: replication.v1.ReplicateResponse
	(*ReplicateCommandsResponse)(nil), // 9: replication.v1.ReplicateCommandsResponse
	(*ReplicateCommand)(nil),          // 10: replication.v1.ReplicateCommand
	(*ReplicateErrResponse)(nil),      // 11: replication.v1.ReplicateErrResponse
	(*Command)(nil),                   // 12: mvcc.v1.Command
}
var file_replication_proto_depIdxs = []int32{
	4,  // 0: replication.v1.MetadataResponse.tables:type_name -> replication.v1.Table
	1,  // 1: replication.v1.Table.type:type_name -> replication.v1.Table.Type
	9,  // 2: replication.v1.ReplicateResponse.commands_response:type_name -> replication.v1.ReplicateCommandsResponse
	11, // 3: replication.v1.ReplicateResponse.error_response:type_name -> replication.v1.ReplicateErrResponse
	10, // 4: replication.v1.ReplicateCommandsResponse.commands:type_name -> replication.v1.ReplicateCommand
	12, // 5: replication.v1.ReplicateCommand.command:type_name -> mvcc.v1.Command
	0,  // 6: replication.v1.ReplicateErrResponse.error:type_name -> replication.v1.ReplicateError
	2,  // 7: replication.v1.Metadata.Get:input_type -> replication.v1.MetadataRequest
	5,  // 8: replication.v1.Snapshot.Stream:input_type -> replication.v1.SnapshotRequest
	7,  // 9: replication.v1.Log.Replicate:input_type -> replication.v1.ReplicateRequest
	3,  // 10: replication.v1.Metadata.Get:output_type -> replication.v1.MetadataResponse
	6,  // 11: replication.v1.Snapshot.Stream:output_type -> replication.v1.SnapshotChunk
	8,  // 12: replication.v1.Log.Replicate:output_type -> replication.v1.ReplicateResponse
	10, // [10:13] is the sub-list for method output_type
	7,  // [7:10] is the sub-list for method input_type
	7,  // [7:7] is the sub-list for extension type_name
	7,  // [7:7] is the sub-list for extension extendee
	0,  // [0:7] is the sub-list for field type_name
}

func init() { file_replication_proto_init() }
func file_replication_proto_init() {
	if File_replication_proto != nil {
		return
	}
	file_mvcc_proto_init()
	file_replication_proto_msgTypes[6].OneofWrappers = []any{
		(*ReplicateResponse_CommandsResponse)(nil),
		(*ReplicateResponse_ErrorResponse)(nil),
	}
	type x struct{}
	out := protoimpl.TypeBuilder{
		File: protoimpl.DescBuilder{
			GoPackagePath: reflect.TypeOf(x{}).PkgPath(),
			RawDescriptor: unsafe.Slice(unsafe.StringData(file_replication_proto_rawDesc), len(file_replication_proto_rawDesc)),
			NumEnums:      2,
			NumMessages:   10,
			NumExtensions: 0,
			NumServices:   3,
		},
		GoTypes:           file_replication_proto_goTypes,
		DependencyIndexes: file_replication_proto_depIdxs,
		EnumInfos:         file_replication_proto_enumTypes,
		MessageInfos:      file_replication_proto_msgTypes,
	}.Build()
	File_replication_proto = out.File
	file_replication_proto_goTypes = nil
	file_replication_proto_depIdxs = nil
}
