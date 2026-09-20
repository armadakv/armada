# Container-based recovery harness

End-to-end verification of inter-cluster snapshot recovery
([RFC 005](../docs/proposals/005-resilient-snapshot-recovery.md)) against two real
three-node Armada clusters: a leader cluster exporting snapshot artefacts to a
shared filesystem store, and a follower cluster recovering from them.

```bash
make test-integration                              # every scenario
make test-integration-one SCENARIO=incremental     # one scenario
cd integration && go test -v -run 'TestRecovery/full' ./...
```

The only prerequisite is a working Docker (or Podman) socket — no `goreman`,
`ghz`, `jq` or `arq` on the host. The image is built from the repository's
`Dockerfile` on the first run, which takes a few minutes; afterwards Docker's
layer cache keys the `COPY . .` layer on content, so an unchanged tree rebuilds
in seconds and an edited one is picked up automatically.

## Why this is a separate module

`testcontainers-go` drags in the Docker/Moby client and its transitive tree.
Keeping the harness in its own module means none of that reaches the main
module's `go.mod`, so `go build ./...` and the unit test suite are unaffected.
Nested modules are also invisible to the repository root's `go build ./...` and
`go test ./...`, so nothing here runs unless it is asked for by name — which is
why there is no build tag on top of that.

The `replace` directive points at `../`, so the harness always tests the working
tree rather than a published release. It imports the product's own packages for
the shared-store `Meta` layout and the artefact-type constants, so a rename over
there breaks this build instead of silently breaking a string match.

## Scenarios

| Subtest | What it establishes |
|---|---|
| `baseline` | Plain tail replication converges the follower, key for key. |
| `full` | A full recovery is learner-first: exactly one node loads the artefact, peers join the recovery shard as non-voting learners, get promoted, and the shard is swapped in exactly once. |
| `incremental` | A follower inside the delta window applies an `incr` artefact into its **live** shard — same shard id, no learner seeding. |
| `powerloss` | SIGKILL of the node actually coordinating a recovery; it must resume from the durable journal rather than abandon and restart. |
| `direct` | The follower reads the shared bucket itself (`--replication.snapshot-source=direct`) instead of proxying through the leader. |

Each scenario runs in its own leader/follower environment. Recovery tests
intentionally wipe data, rebuild containers, change configuration, and kill
processes; isolation prevents those destructive side effects from contaminating
the next scenario. Every scenario seeds its own converged baseline, so it is
also individually runnable.

## Layout

| File | Contents |
|---|---|
| `config.go` | Every knob, with defaults that pass on a laptop. |
| `image.go` | Builds the Armada image from the working tree, once per process. |
| `cluster.go` | Containers, the harness network, node lifecycle (stop / SIGKILL / revive / rebuild), log retrieval. |
| `client.go` | Typed gRPC data plane: table creation, readiness, the load generator, key-exact verification. |
| `store.go` | Typed shared-store inspection and independent SHA-256 verification of published artefacts. |
| `assert.go` | Polling, scenario-scoped log marks, and structured readings of the recovery log lines. |
| `logging.go` | Harness and testcontainers output in the same layout the armada processes log in. |
| `recovery_test.go` | The scenarios. |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `ARMADA_TEST_IMAGE` | *(build from source)* | Use a prebuilt image instead of building. |
| `ARMADA_TEST_ROOT` | *(test temp dir)* | Parent directory for scenario-specific data, shared stores, and logs. Set it to keep the trees after the run. |
| `ARMADA_TEST_KEEP` | unset | Leave the containers running afterwards, for manual poking. |
| `ARMADA_TEST_TABLE` | `armada-test` | Table under test. |
| `ARMADA_TEST_KEYS` | `2000` | Size of the baseline data set. |
| `ARMADA_TEST_INCR_GAP` | `4000` | Upper bound on how far the follower may be pushed behind while the incremental scenario hunts for the delta window. |
| `ARMADA_TEST_INCR_CHUNK` | `150` | Keys written between shared-store checks while hunting. |
| `ARMADA_TEST_CONCURRENCY` | `32` | In-flight `Put`s in the loader. |
| `ARMADA_TEST_PORT_BASE` | `15000` | Base of the fixed published host ports. |
| `ARMADA_TEST_SUBNET` | `10.99.0.0/16` | Harness network subnet. |
| `ARMADA_TEST_TIMEOUT_SCALE` | `1s` | Scales every wait in the suite; `2s` doubles them. |

Raft and shared-store tuning (`ARMADA_TEST_LEADER_SNAPSHOT_ENTRIES`,
`ARMADA_TEST_FULL_INTERVAL`, `ARMADA_TEST_INCR_MAX_CHAIN`, …) is listed in
`config.go`. The defaults are deliberately aggressive so that log compaction —
and therefore the GC horizon — moves within seconds rather than hours.

### Leader raft tuning

The leader runs `snapshot-entries=1000`, `compaction-overhead=500`: the shipped
defaults (`10000/5000`, `cmd/armada/flags.go`) divided by ten, so that a few
thousand writes produce several compactions rather than none. The **2:1 ratio**
is preserved deliberately, because the ratio is the part that decides whether
incremental artefacts exist at all.

The exporter refuses to publish a delta whose base is at or below the GC
horizon, and it used to chain the base onto the previous artefact's tip. Exports
are driven by log compaction, so that tip sits roughly `snapshot-entries` behind
the current index while the horizon trails it by roughly
`compaction-overhead` — so a delta was only publishable when

```
snapshot-entries < compaction-overhead
```

Every realistic configuration has that the other way round, so *every* export
was forced to a full and the incremental path was unreachable rather than merely
rare. This harness originally ran an inverted `100/500` to exercise it at all,
which is how the problem was found. `ExportIncremental` now re-bases at the
horizon instead of giving up, so deltas are publishable at any ratio; keeping
the production ratio here is what keeps that honest — under the old behaviour
these values reproduce the failure.

After the fix a delta spans roughly `compaction-overhead` entries, which is also
how wide the window is for a lagging follower to be served incrementally: 5000
entries at the shipped defaults, 500 at the harness's scale.

The `incremental` scenario still restarts the leader with
`shared-store.full-interval=30m` for the duration of its setup. The interval
ticker publishes fulls unconditionally, and one landing past the stopped
follower shuts the delta window for good.

### Pinned hostnames

Each container's hostname is pinned to its logical node name (`leader1`,
`follower2`, …). Dragonboat records `os.Hostname()` in the NodeHost directory and
refuses to open it under a different one — a guard against a data directory
being moved between machines. Docker otherwise derives the hostname from the
container id, so recreating a container over a surviving data directory looks
exactly like that move and the node dies at startup with `hostname changed`.
Restarting a container keeps its id, so this only bites after a rebuild.

### Fixed ports, static addresses

Host ports are fixed rather than ephemeral: `15001-15003` leader API,
`15011-15013` leader admin, `15021-15023` follower API, `15031-15033` follower
admin. Two reasons: a Docker restart reassigns an ephemeral port, which would
break every cached client mid-scenario; and with `ARMADA_TEST_KEEP` you can
point `arq`/`arctl` at a paused harness.

```bash
ARMADA_TEST_KEEP=1 ARMADA_TEST_ROOT=/tmp/armada-it make test-integration-one SCENARIO=full
./arq --address 127.0.0.1:15001 --table armada-test get --all --count-only
./arq --address 127.0.0.1:15021 --table armada-test get --all --count-only
```

Each container also gets a static address on the harness network (leaders
`.11-.13`, followers `.21-.23`), so `--raft.initial-members` is a list of plain
IPs exactly as in a real deployment. It has to be: a node's replica ID is its
1-based position in that list, so the addresses must be known before anything
starts.

## Differences from `hack/recovery-e2e.sh`

The shell harness is still there and still works; this one is not a
transliteration of it. What changed:

- **No external tools.** Load generation, key counting and table creation go
  through the generated gRPC clients, so there is no `ghz` templating, no
  base64-in-JSON, and no parsing of `arq` output. A failed write is an error
  value, not an absent "Error distribution" section in a report.
- **Per-node attribution.** The shell version concatenated all three follower
  logs into one file, so "exactly one node loaded the snapshot" could not
  actually be checked. Here each container's log is separate, and assertions
  report which node matched.
- **Structured log readings.** The negotiated artefact and the completed swaps
  are parsed into `Negotiation` and `Swap` values rather than substring-matched,
  and the artefact type comes from the product's own constants — a rename breaks
  the build instead of silently breaking a grep.
- **Key-exact verification.** `VerifyKeys` checks every key and value and
  reports gaps as contiguous runs (`200-219 (20 keys)`). A count alone cannot
  distinguish a correct recovery from one that dropped one range and duplicated
  another, which is exactly the shape a delta exported below the GC horizon
  produces.
- **Independent artefact verification.** `CheckStore` re-hashes every published
  `.snap` against its own `.meta`. The follower verifies what it received; this
  verifies what was published, which tells a bad upload apart from a bad
  download.
- **The incremental window is opened by measurement, not by a fixed recipe.**
  The scenario writes in small chunks and watches the shared store until a
  follower stopped at index *F* both **must** recover (it is below the leader's
  GC horizon) and **can** (a delta covers it), then stops writing. It mirrors
  `SelectBestSnapshot`, the export-side horizon guard and `ResumableSnapshots`
  to decide that, so once the window is open it *requires* the incremental path
  rather than hoping for it. When the window cannot be opened it skips with the
  numbers it measured — which artefact tips exist, where the horizon is — so the
  next investigation starts from data. The shell version wrote a fixed 1400 keys
  in one go and could only warn, with no way to tell "the path is broken" from
  "the window never opened".
- **The power-loss victim is the real coordinator.** It is identified from the
  per-node logs rather than by picking the first running node, so the scenario
  actually exercises journal resume instead of usually killing a bystander.
- **One log format.** Harness progress, testcontainers' container bookkeeping
  and the armada processes' own zap output all share the
  `TIMESTAMP<TAB>LEVEL<TAB>NAME<TAB>MESSAGE` layout, and the harness lines go
  through `t.Output()` so they interleave with the test log in order instead of
  racing it on stderr. A run's output and the six saved container logs can be
  merged with `sort` into one timeline.

## Reading a failure

Container logs are saved per node under
`$ARMADA_TEST_ROOT/TestRecovery-<scenario>/logs/` (or the test's temporary
directory) on every run, passing or failing. Scenario roots are isolated so a
failure preserves the exact cluster state that produced it.

```bash
ARMADA_TEST_ROOT=/tmp/armada-it make test-integration
sort -m /tmp/armada-it/TestRecovery-full/logs/follower*.log
```
