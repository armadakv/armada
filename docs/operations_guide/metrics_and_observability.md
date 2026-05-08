# Metrics and Observability

## Health Check

Armada exposes a `/healthz` endpoint on the REST API port (default `8079`):

```
GET http://127.0.0.1:8079/healthz
```

A `200 OK` response indicates the server is healthy.

## Metrics

Armada exposes metrics for Prometheus, available via the `/metrics` endpoint in the REST API (default port 8079).
Go runtime statistics, gRPC statistics, and Raft statistics are exposed.

### Key Metrics

| Metric | Labels | Description |
|--------|--------|-------------|
| `dragonboat_raftnode_has_leader` | `shardid`, `replicaid` | `1` when this node has a Raft leader for the given shard |
| `grpc_server_handled_total` | `grpc_code`, `grpc_method`, `grpc_service`, `grpc_type` | Total number of completed gRPC calls, labelled by result code |
| `grpc_server_handling_seconds` | `grpc_method`, `grpc_service`, `grpc_type` | Histogram of gRPC call durations |
| `regatta_table_storage_cache_hits` | `clusterID`, `table`, `type` | Block/table cache hits for a given table's storage |
| `regatta_table_storage_cache_misses` | `clusterID`, `table`, `type` | Block/table cache misses for a given table's storage |
| `regatta_table_storage_read_amp` | `clusterID`, `table` | Read amplification factor for a given table |
| `regatta_table_storage_write_amp` | `clusterID`, `table` | Write amplification factor for a given table |

> **Note:** The gRPC service names and `regatta_*` metric prefixes use the legacy `regatta` naming.
> This will be updated in a future release.

### Example Queries

```promql
# Fraction of Range requests that return an error
sum(rate(grpc_server_handled_total{grpc_method="Range", grpc_code!="OK"}[5m]))
  / sum(rate(grpc_server_handled_total{grpc_method="Range"}[5m]))

# p99 latency of Put requests
histogram_quantile(0.99,
  sum by (le) (rate(grpc_server_handling_seconds_bucket{grpc_method="Put"}[5m]))
)
```

## Alerts

Prometheus alerting rules can be found in the
[Helm Chart](https://github.com/armadakv/armada-helm/blob/3dc1954d2a08c4a983c7cef0c2e853bfa5ef65aa/charts/regatta/values.yaml#L467).

## Debugging

Armada also exposes the `/debug` endpoint in the REST API for runtime profiling via
[pprof](https://github.com/google/pprof).

```bash
# Download a 30-second CPU profile
go tool pprof http://127.0.0.1:8079/debug/pprof/profile?seconds=30
```
