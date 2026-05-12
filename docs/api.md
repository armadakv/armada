---
title: "API Reference"
subtitle: "Complete gRPC API documentation"
description: "Full reference for ArmadaKV's gRPC APIs: KV, Replication, Maintenance, and Tables."
section: "overview"
order: 4
---

# gRPC API Reference


## armada.proto


### Service: Cluster

Cluster service ops.

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **MemberList** | [MemberListRequest](#memberlistrequest) | [MemberListResponse](#memberlistresponse) | MemberList lists all the members in the cluster. |
| **Status** | [StatusRequest](#statusrequest) | [StatusResponse](#statusresponse) | Status gets the status of the member. |


### Service: KV

KV for handling the read/put requests

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Range** | [RangeRequest](#rangerequest) | [RangeResponse](#rangeresponse) | Range gets the keys in the range from the key-value store. |
| **IterateRange** | [RangeRequest](#rangerequest) | [RangeResponse](#rangeresponse) | IterateRange gets the keys in the range from the key-value store. |
| **Put** | [PutRequest](#putrequest) | [PutResponse](#putresponse) | Put puts the given key into the key-value store. |
| **DeleteRange** | [DeleteRangeRequest](#deleterangerequest) | [DeleteRangeResponse](#deleterangeresponse) | DeleteRange deletes the given range from the key-value store. |
| **Txn** | [TxnRequest](#txnrequest) | [TxnResponse](#txnresponse) | Txn processes multiple requests in a single transaction. A txn request increments the revision of the key-value store and generates events with the same revision for every completed request. It is allowed to modify the same key several times within one txn (the result will be the last Op that modified the key). |


### Service: Tables

API for managing tables.

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Create** | [CreateTableRequest](#createtablerequest) | [CreateTableResponse](#createtableresponse) | Create a table. All followers will automatically replicate the table. This procedure is available only in the leader cluster. |
| **Delete** | [DeleteTableRequest](#deletetablerequest) | [DeleteTableResponse](#deletetableresponse) | Delete a table. All followers will automatically delete the table. This procedure is available only in the leader cluster. |
| **List** | [ListTablesRequest](#listtablesrequest) | [ListTablesResponse](#listtablesresponse) | Get names of all the tables present in the cluster. This procedure is available in both leader and follower clusters. |



#### CreateTableRequest {#createtablerequest}

CreateTableRequest describes the table to be created.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `name` | [string](#string) |  | Name of the table to be created. |
| `config` | [google.protobuf.Struct](#googleprotobufstruct) |  | config the table configuration values. |


#### CreateTableResponse {#createtableresponse}

CreateTableResponse describes the newly created table.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `id` | [string](#string) |  | id the created table. |


#### DeleteRangeRequest {#deleterangerequest}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table name of the table |
| `key` | [bytes](#bytes) |  | key is the first key to delete in the range. |
| `range_end` | [bytes](#bytes) |  | range_end is the key following the last key to delete for the range [key, range_end). If range_end is not given, the range is defined to contain only the key argument. If range_end is one bit larger than the given key, then the range is all the keys with the prefix (the given key). If range_end is '\0', the range is all keys greater than or equal to the key argument. |
| `prev_kv` | [bool](#bool) |  | If prev_kv is set, regatta gets the previous key-value pairs before deleting it. The previous key-value pairs will be returned in the delete response. Beware that getting previous records could have serious performance impact on a delete range spanning a large dataset. |
| `count` | [bool](#bool) |  | If count is set, regatta gets the count of previous key-value pairs before deleting it. The deleted field will be set to number of deleted key-value pairs in the response. Beware that counting records could have serious performance impact on a delete range spanning a large dataset. |


#### DeleteRangeResponse {#deleterangeresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `header` | [ResponseHeader](#responseheader) |  |  |
| `deleted` | [int64](#int64) |  | deleted is the number of keys deleted by the delete range request. |
| `prev_kvs` | [mvcc.v1.KeyValue](#mvccv1keyvalue) | repeated | if prev_kv is set in the request, the previous key-value pairs will be returned. |


#### DeleteTableRequest {#deletetablerequest}

DeleteTableRequest describes the table to be deleted.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `name` | [string](#string) |  | name of the table to be deleted. |


#### DeleteTableResponse {#deletetableresponse}

DeleteTableResponse when the table was successfully deleted.



#### ListTablesRequest {#listtablesrequest}

ListTablesRequest requests the list of currently registered tables.



#### ListTablesResponse {#listtablesresponse}

FollowerGetTablesResponse contains information about tables stored in the cluster.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `tables` | [TableInfo](#tableinfo) | repeated |  |


#### Member {#member}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `id` | [string](#string) |  | id is the member ID of this member. |
| `name` | [string](#string) |  | name is the human-readable name of the member. If the member is not started, the name will be an empty string. |
| `peerURLs` | [string](#string) | repeated | peerURLs is the list of URLs the member exposes to the cluster for communication. |
| `clientURLs` | [string](#string) | repeated | clientURLs is the list of URLs the member exposes to clients for communication. If the member is not started, clientURLs will be empty. |


#### MemberListRequest {#memberlistrequest}



#### MemberListResponse {#memberlistresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `cluster` | [string](#string) |  | cluster is a name of the cluster. |
| `members` | [Member](#member) | repeated | members is a list of all members associated with the cluster. |


#### PutRequest {#putrequest}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table name of the table |
| `key` | [bytes](#bytes) |  | key is the key, in bytes, to put into the key-value store. |
| `value` | [bytes](#bytes) |  | value is the value, in bytes, to associate with the key in the key-value store. |
| `prev_kv` | [bool](#bool) |  | prev_kv if true the previous key-value pair will be returned in the put response. |


#### PutResponse {#putresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `header` | [ResponseHeader](#responseheader) |  |  |
| `prev_kv` | [mvcc.v1.KeyValue](#mvccv1keyvalue) |  | if prev_kv is set in the request, the previous key-value pair will be returned. |


#### RangeRequest {#rangerequest}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table name of the table |
| `key` | [bytes](#bytes) |  | key is the first key for the range. If range_end is not given, the request only looks up key. |
| `range_end` | [bytes](#bytes) |  | range_end is the upper bound on the requested range [key, range_end). If range_end is '\0', the range is all keys >= key. If range_end is key plus one (e.g., "aa"+1 == "ab", "a\xff"+1 == "b"), then the range request gets all keys prefixed with key. If both key and range_end are '\0', then the range request returns all keys. |
| `limit` | [int64](#int64) |  | limit is a limit on the number of keys returned for the request. When limit is set to 0, it is treated as no limit. |
| `linearizable` | [bool](#bool) |  | linearizable sets the range request to use linearizable read. Linearizable requests have higher latency and lower throughput than serializable requests but reflect the current consensus of the cluster. For better performance, in exchange for possible stale reads, a serializable range request is served locally without needing to reach consensus with other nodes in the cluster. The serializable request is default option. |
| `keys_only` | [bool](#bool) |  | keys_only when set returns only the keys and not the values. |
| `count_only` | [bool](#bool) |  | count_only when set returns only the count of the keys in the range. |
| `min_mod_revision` | [int64](#int64) |  | min_mod_revision is the lower bound for returned key mod revisions; all keys with lesser mod revisions will be filtered away. |
| `max_mod_revision` | [int64](#int64) |  | max_mod_revision is the upper bound for returned key mod revisions; all keys with greater mod revisions will be filtered away. |
| `min_create_revision` | [int64](#int64) |  | min_create_revision is the lower bound for returned key create revisions; all keys with lesser create revisions will be filtered away. |
| `max_create_revision` | [int64](#int64) |  | max_create_revision is the upper bound for returned key create revisions; all keys with greater create revisions will be filtered away. |


#### RangeResponse {#rangeresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `header` | [ResponseHeader](#responseheader) |  |  |
| `kvs` | [mvcc.v1.KeyValue](#mvccv1keyvalue) | repeated | kvs is the list of key-value pairs matched by the range request. kvs is empty when count is requested. |
| `more` | [bool](#bool) |  | more indicates if there are more keys to return in the requested range. |
| `count` | [int64](#int64) |  | count is set to the number of keys within the range when requested. |


#### ResponseHeader {#responseheader}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `shard_id` | [uint64](#uint64) |  | shard_id is the ID of the shard which sent the response. |
| `replica_id` | [uint64](#uint64) |  | replica_id is the ID of the member which sent the response. |
| `revision` | [uint64](#uint64) |  | revision is the key-value store revision when the request was applied. |
| `raft_term` | [uint64](#uint64) |  | raft_term is the raft term when the request was applied. |
| `raft_leader_id` | [uint64](#uint64) |  | raft_leader_id is the ID of the actual raft quorum leader. |


#### StatusRequest {#statusrequest}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `config` | [bool](#bool) |  | config controls if the configuration values should be fetched as well. |


#### StatusResponse {#statusresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `id` | [string](#string) |  | id is the member ID of this member. |
| `version` | [string](#string) |  | version is the semver version used by the responding member. |
| `info` | [string](#string) |  | info is the additional server info. |
| `tables` | regatta.v1.StatusResponse.TablesEntry |  | tables is a status of tables of the responding member. |
| `config` | [google.protobuf.Struct](#googleprotobufstruct) |  | config the node configuration values. |
| `errors` | [string](#string) | repeated | errors contains alarm/health information and status. |


#### StatusResponse.TablesEntry {#statusresponsetablesentry}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `key` | [string](#string) |  |  |
| `value` | [TableStatus](#tablestatus) |  |  |


#### TableInfo {#tableinfo}

TableInfo describes a single table.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `name` | [string](#string) |  | name of the table. |
| `id` | [string](#string) |  | id of the table. |
| `config` | [google.protobuf.Struct](#googleprotobufstruct) |  | config the table configuration values. |


#### TableStatus {#tablestatus}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `leader` | [string](#string) |  | leader is the member ID which the responding member believes is the current leader. |
| `raftIndex` | [uint64](#uint64) |  | raftIndex is the current raft committed index of the responding member. |
| `raftTerm` | [uint64](#uint64) |  | raftTerm is the current raft term of the responding member. |
| `raftAppliedIndex` | [uint64](#uint64) |  | raftAppliedIndex is the current raft applied index of the responding member. |


#### TxnRequest {#txnrequest}

From google paxosdb paper:
Our implementation hinges around a powerful primitive which we call MultiOp. All other database
operations except for iteration are implemented as a single call to MultiOp. A MultiOp is applied atomically
and consists of three components:
1. A list of tests called guard. Each test in guard checks a single entry in the database. It may check
for the absence or presence of a value, or compare with a given value. Two different tests in the guard
may apply to the same or different entries in the database. All tests in the guard are applied and
MultiOp returns the results. If all tests are true, MultiOp executes t op (see item 2 below), otherwise
it executes f op (see item 3 below).
2. A list of database operations called t op. Each operation in the list is either an insert, delete, or
lookup operation, and applies to a database entry(ies). Two different operations in the list may apply
to the same or different entries in the database. These operations are executed
if guard evaluates to true.
3. A list of database operations called f op. Like t op, but executed if guard evaluates to false.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table name of the table |
| `compare` | [mvcc.v1.Compare](#mvccv1compare) | repeated | compare is a list of predicates representing a conjunction of terms. If the comparisons succeed, then the success requests will be processed in order, and the response will contain their respective responses in order. If the comparisons fail, then the failure requests will be processed in order, and the response will contain their respective responses in order. |
| `success` | [mvcc.v1.RequestOp](#mvccv1requestop) | repeated | success is a list of requests which will be applied when compare evaluates to true. |
| `failure` | [mvcc.v1.RequestOp](#mvccv1requestop) | repeated | failure is a list of requests which will be applied when compare evaluates to false. |


#### TxnResponse {#txnresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `header` | [ResponseHeader](#responseheader) |  |  |
| `succeeded` | [bool](#bool) |  | succeeded is set to true if the compare evaluated to true or false otherwise. |
| `responses` | [mvcc.v1.ResponseOp](#mvccv1responseop) | repeated | responses is a list of responses corresponding to the results from applying success if succeeded is true or failure if succeeded is false. |





## maintenance.proto


### Service: Maintenance

Maintenance service provides methods for maintenance purposes.

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Backup** | [BackupRequest](#backuprequest) | [.replication.v1.SnapshotChunk](#replicationv1snapshotchunk) |  |
| **Restore** | [RestoreMessage](#restoremessage) | [RestoreResponse](#restoreresponse) | Restore streams backup data to the server to restore one or more tables from a backup. The stream must begin with a RestoreMessage containing RestoreInfo, followed by one or more RestoreMessages containing SnapshotChunk data. This is a destructive operation — existing table data is replaced by the backup. |
| **Reset** | [ResetRequest](#resetrequest) | [ResetResponse](#resetresponse) |  |



#### BackupRequest {#backuprequest}

BackupRequest requests and opens a stream with backup data.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table is name of the table to stream. |


#### ResetRequest {#resetrequest}

ResetRequest resets either a single or multiple tables in the cluster, meaning that their data will be repopulated from the Leader.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table is a table name to reset. |
| `reset_all` | [bool](#bool) |  | reset_all if true all the tables will be reset, use with caution. |


#### ResetResponse {#resetresponse}



#### RestoreInfo {#restoreinfo}

RestoreInfo metadata of restore snapshot that is going to be uploaded.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table is name of the table in the stream. |


#### RestoreMessage {#restoremessage}

RestoreMessage contains either info of the table being restored or chunk of a backup data.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `info` | [RestoreInfo](#restoreinfo) |  |  |
| `chunk` | [replication.v1.SnapshotChunk](#replicationv1snapshotchunk) |  |  |


#### RestoreResponse {#restoreresponse}






## metrics.proto


### Service: Metrics

Metrics service for retrieving Prometheus metrics data via gRPC

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **GetMetrics** | [MetricsRequest](#metricsrequest) | [MetricsResponse](#metricsresponse) | GetMetrics returns all available Prometheus metrics data. |



#### MetricsRequest {#metricsrequest}

MetricsRequest is used to request metrics data.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `format` | [string](#string) |  | format can be used to specify the desired format of metrics (default: prometheus text format) |


#### MetricsResponse {#metricsresponse}

MetricsResponse contains the requested metrics data.

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `metrics_data` | [string](#string) |  | metrics_data contains the metrics data in the requested format (typically prometheus text format) |
| `timestamp` | [int64](#int64) |  | timestamp represents when these metrics were collected |





## mvcc.proto



#### Command {#command}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table name of the table |
| `type` | [Command.CommandType](#commandcommandtype) |  | type is the kind of event. If type is a PUT, it indicates new data has been stored to the key. If type is a DELETE, it indicates the key was deleted. |
| `kv` | [KeyValue](#keyvalue) |  | kv holds the KeyValue for the event. A PUT event contains current kv pair. A PUT event with kv.Version=1 indicates the creation of a key. A DELETE/EXPIRE event contains the deleted key with its modification revision set to the revision of deletion. |
| `leader_index` | [uint64](#uint64) | optional | leader_index holds the value of the log index of a leader cluster from which this command was replicated from. |
| `batch` | [KeyValue](#keyvalue) | repeated | batch is an atomic batch of KVs to either PUT or DELETE. (faster, no read, no mix of types, no conditions). |
| `txn` | [Txn](#txn) | optional | txn is an atomic transaction (slow, supports reads and conditions). |
| `range_end` | [bytes](#bytes) | optional | range_end is the key following the last key to affect for the range [kv.key, range_end). If range_end is not given, the range is defined to contain only the kv.key argument. If range_end is one bit larger than the given kv.key, then the range is all the keys with the prefix (the given key). If range_end is '\0', the range is all keys greater than or equal to the key argument. |
| `prev_kvs` | [bool](#bool) |  | prev_kvs if to fetch previous KVs. |
| `sequence` | [Command](#command) | repeated | sequence is the sequence of commands to be applied as a single FSM step. |
| `count` | [bool](#bool) |  | count if to count number of records affected by a command. |


#### CommandResult {#commandresult}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `responses` | [ResponseOp](#responseop) | repeated | responses are the responses (if any) in order of application. |
| `revision` | [uint64](#uint64) |  | revision is the key-value store revision when the request was applied. |


#### Compare {#compare}

Compare property `target` for every KV from DB in [key, range_end) with target_union using the operation `result`. e.g. `DB[key].target result target_union.target`,
that means that for asymmetric operations LESS and GREATER the target property of the key from the DB is the left-hand side of the comparison.
Examples:
* `DB[key][value] EQUAL target_union.value`
* `DB[key][value] GREATER target_union.value`
* `DB[key...range_end][value] GREATER target_union.value`
* `DB[key][value] LESS target_union.value`

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `result` | [Compare.CompareResult](#comparecompareresult) |  | result is logical comparison operation for this comparison. |
| `target` | [Compare.CompareTarget](#comparecomparetarget) |  | target is the key-value field to inspect for the comparison. |
| `key` | [bytes](#bytes) |  | key is the subject key for the comparison operation. |
| `value` | [bytes](#bytes) |  | value is the value of the given key, in bytes. |
| `range_end` | [bytes](#bytes) |  | range_end compares the given target to all keys in the range [key, range_end). See RangeRequest for more details on key ranges.  TODO: fill out with most of the rest of RangeRequest fields when needed. |


#### KeyValue {#keyvalue}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `key` | [bytes](#bytes) |  | key is the key in bytes. An empty key is not allowed. |
| `create_revision` | [int64](#int64) |  | create_revision is the revision of last creation on this key. |
| `mod_revision` | [int64](#int64) |  | mod_revision is the revision of last modification on this key. |
| `value` | [bytes](#bytes) |  | value is the value held by the key, in bytes. |


#### RequestOp {#requestop}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `request_range` | [RequestOp.Range](#requestoprange) |  |  |
| `request_put` | [RequestOp.Put](#requestopput) |  |  |
| `request_delete_range` | [RequestOp.DeleteRange](#requestopdeleterange) |  |  |


#### RequestOp.DeleteRange {#requestopdeleterange}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `key` | [bytes](#bytes) |  | key is the first key to delete in the range. |
| `range_end` | [bytes](#bytes) |  | range_end is the key following the last key to delete for the range [key, range_end). If range_end is not given, the range is defined to contain only the key argument. If range_end is one bit larger than the given key, then the range is all the keys with the prefix (the given key). If range_end is '\0', the range is all keys greater than or equal to the key argument. |
| `prev_kv` | [bool](#bool) |  | If prev_kv is set, regatta gets the previous key-value pairs before deleting it. The previous key-value pairs will be returned in the delete response. Beware that getting previous records could have serious performance impact on a delete range spanning a large dataset. |
| `count` | [bool](#bool) |  | If count is set, regatta gets the count of previous key-value pairs before deleting it. The deleted field will be set to number of deleted key-value pairs in the response. Beware that counting records could have serious performance impact on a delete range spanning a large dataset. |


#### RequestOp.Put {#requestopput}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `key` | [bytes](#bytes) |  | key is the key, in bytes, to put into the key-value store. |
| `value` | [bytes](#bytes) |  | value is the value, in bytes, to associate with the key in the key-value store. |
| `prev_kv` | [bool](#bool) |  | prev_kv if true the previous key-value pair will be returned in the put response. |


#### RequestOp.Range {#requestoprange}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `key` | [bytes](#bytes) |  | key is the first key for the range. If range_end is not given, the request only looks up key. |
| `range_end` | [bytes](#bytes) |  | range_end is the upper bound on the requested range [key, range_end). If range_end is '\0', the range is all keys >= key. If range_end is key plus one (e.g., "aa"+1 == "ab", "a\xff"+1 == "b"), then the range request gets all keys prefixed with key. If both key and range_end are '\0', then the range request returns all keys. |
| `limit` | [int64](#int64) |  | limit is a limit on the number of keys returned for the request. When limit is set to 0, it is treated as no limit. |
| `keys_only` | [bool](#bool) |  | keys_only when set returns only the keys and not the values. |
| `count_only` | [bool](#bool) |  | count_only when set returns only the count of the keys in the range. |


#### ResponseOp {#responseop}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `response_range` | [ResponseOp.Range](#responseoprange) |  |  |
| `response_put` | [ResponseOp.Put](#responseopput) |  |  |
| `response_delete_range` | [ResponseOp.DeleteRange](#responseopdeleterange) |  |  |


#### ResponseOp.DeleteRange {#responseopdeleterange}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `deleted` | [int64](#int64) |  | deleted is the number of keys deleted by the delete range request. |
| `prev_kvs` | [KeyValue](#keyvalue) | repeated | if prev_kv is set in the request, the previous key-value pairs will be returned. |


#### ResponseOp.Put {#responseopput}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `prev_kv` | [KeyValue](#keyvalue) |  | if prev_kv is set in the request, the previous key-value pair will be returned. |


#### ResponseOp.Range {#responseoprange}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `kvs` | [KeyValue](#keyvalue) | repeated | kvs is the list of key-value pairs matched by the range request. kvs is empty when count is requested. |
| `more` | [bool](#bool) |  | more indicates if there are more keys to return in the requested range. |
| `count` | [int64](#int64) |  | count is set to the number of keys within the range when requested. |


#### Txn {#txn}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `compare` | [Compare](#compare) | repeated | compare is a list of predicates representing a conjunction of terms. If the comparisons succeed, then the success requests will be processed in order, and the response will contain their respective responses in order. If the comparisons fail, then the failure requests will be processed in order, and the response will contain their respective responses in order. |
| `success` | [RequestOp](#requestop) | repeated | success is a list of requests which will be applied when compare evaluates to true. |
| `failure` | [RequestOp](#requestop) | repeated | failure is a list of requests which will be applied when compare evaluates to false. |



#### Command.CommandType {#commandcommandtype}

| Name | Number | Description |
| ---- | ------ | ----------- |
| `PUT` | 0 |  |
| `DELETE` | 1 |  |
| `DUMMY` | 2 |  |
| `PUT_BATCH` | 3 |  |
| `DELETE_BATCH` | 4 |  |
| `TXN` | 5 |  |
| `SEQUENCE` | 6 |  |
| `GC` | 7 |  |



#### Compare.CompareResult {#comparecompareresult}

| Name | Number | Description |
| ---- | ------ | ----------- |
| `EQUAL` | 0 |  |
| `GREATER` | 1 |  |
| `LESS` | 2 |  |
| `NOT_EQUAL` | 3 |  |



#### Compare.CompareTarget {#comparecomparetarget}

| Name | Number | Description |
| ---- | ------ | ----------- |
| `VALUE` | 0 |  |





## replication.proto


### Service: Log

Log service provides methods to replicate data from Armada leader's log to Armada followers' logs.

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Replicate** | [ReplicateRequest](#replicaterequest) | [ReplicateResponse](#replicateresponse) | Replicate is method to ask for data of specified table from the specified index. |


### Service: Metadata

Metadata service provides method to get Armada metadata, e.g. tables.

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Get** | [MetadataRequest](#metadatarequest) | [MetadataResponse](#metadataresponse) |  |


### Service: Snapshot

| Method | Request | Response | Description |
| ------ | ------- | -------- | ----------- |
| **Stream** | [SnapshotRequest](#snapshotrequest) | [SnapshotChunk](#snapshotchunk) |  |



#### MetadataRequest {#metadatarequest}



#### MetadataResponse {#metadataresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `tables` | [Table](#table) | repeated |  |


#### ReplicateCommand {#replicatecommand}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `leader_index` | [uint64](#uint64) |  | leaderIndex represents the leader raft index of the given command |
| `command` | [mvcc.v1.Command](#mvccv1command) |  | command holds the leader raft log command at leaderIndex |


#### ReplicateCommandsResponse {#replicatecommandsresponse}

ReplicateCommandsResponse sequence of replication commands

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `commands` | [ReplicateCommand](#replicatecommand) | repeated | commands represent the |


#### ReplicateErrResponse {#replicateerrresponse}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `error` | [ReplicateError](#replicateerror) |  |  |


#### ReplicateRequest {#replicaterequest}

ReplicateRequest request of the replication data at given leader_index

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table is name of the table to replicate |
| `leader_index` | [uint64](#uint64) |  | leader_index is the index in the leader raft log of the last stored item in the follower |


#### ReplicateResponse {#replicateresponse}

ReplicateResponse response to the ReplicateRequest

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `commands_response` | [ReplicateCommandsResponse](#replicatecommandsresponse) |  |  |
| `error_response` | [ReplicateErrResponse](#replicateerrresponse) |  |  |
| `leader_index` | [uint64](#uint64) |  | leader_index is the largest applied leader index at the time of the client RPC. |


#### SnapshotChunk {#snapshotchunk}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `data` | [bytes](#bytes) |  | data is chunk of snapshot |
| `len` | [uint64](#uint64) |  | len is a length of data bytes |
| `index` | [uint64](#uint64) |  | index the index for which the snapshot was created |


#### SnapshotRequest {#snapshotrequest}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `table` | [bytes](#bytes) |  | table is name of the table to stream |
| `incremental` | [bool](#bool) |  | incremental indicates that the client wants an incremental snapshot starting from leader_index. If false, a full snapshot is produced regardless of leader_index. |
| `leader_index` | [uint64](#uint64) |  | leader_index is the last leader index the follower already has applied. Only meaningful when incremental is true. The server will stream only the changes (puts and deletes) with seqno > leader_index. |


#### Table {#table}

| Field | Type | Label | Description |
| ----- | ---- | ----- | ----------- |
| `name` | [string](#string) |  |  |
| `type` | [Table.Type](#tabletype) |  |  |



#### ReplicateError {#replicateerror}

| Name | Number | Description |
| ---- | ------ | ----------- |
| `USE_SNAPSHOT` | 0 | USE_SNAPSHOT occurs when leader has no longer the specified `leader_index` in the log. Follower must use `GetSnapshot` to catch up. |
| `LEADER_BEHIND` | 1 | LEADER_BEHIND occurs when the index of the leader is smaller than requested `leader_index`. This should never happen. Manual intervention needed. |



#### Table.Type {#tabletype}

| Name | Number | Description |
| ---- | ------ | ----------- |
| `REPLICATED` | 0 | REPLICATED indicates a table that is replicated from the leader cluster to this follower cluster. |
| `LOCAL` | 1 | LOCAL indicates a table that exists only in this cluster and is not replicated from a leader. |




## Scalar Value Types

| Type | Notes | Go | Java | Python |
| ---- | ----- | -- | ---- | ------ |
| `double` |  | `float64` | `double` | `float` |
| `float` |  | `float32` | `float` | `float` |
| `int32` | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint32 instead. | `int32` | `int` | `int` |
| `int64` | Uses variable-length encoding. Inefficient for encoding negative numbers – if your field is likely to have negative values, use sint64 instead. | `int64` | `long` | `int/long` |
| `uint32` | Uses variable-length encoding. | `uint32` | `int` | `int/long` |
| `uint64` | Uses variable-length encoding. | `uint64` | `long` | `int/long` |
| `sint32` | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int32s. | `int32` | `int` | `int` |
| `sint64` | Uses variable-length encoding. Signed int value. These more efficiently encode negative numbers than regular int64s. | `int64` | `long` | `int/long` |
| `fixed32` | Always four bytes. More efficient than uint32 if values are often greater than 2^28. | `uint32` | `int` | `int` |
| `fixed64` | Always eight bytes. More efficient than uint64 if values are often greater than 2^56. | `uint64` | `long` | `int/long` |
| `sfixed32` | Always four bytes. | `int32` | `int` | `int` |
| `sfixed64` | Always eight bytes. | `int64` | `long` | `int/long` |
| `bool` |  | `bool` | `boolean` | `boolean` |
| `string` | A string must always contain UTF-8 encoded or 7-bit ASCII text. | `string` | `String` | `str/unicode` |
| `bytes` | May contain any arbitrary sequence of bytes. | `[]byte` | `ByteString` | `str` |

