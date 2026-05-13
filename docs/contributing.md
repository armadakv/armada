---
title: "Contributing"
description: "How to contribute to ArmadaKV — code, docs, questions, and community guidelines."
section: "overview"
order: 7
---

# Contributing

Armada welcomes contributions of all kinds — bug reports, feature requests, documentation
improvements, and code changes. If you would like to get involved, feel free to reach out
in [GitHub Discussions](https://github.com/armadakv/armada/discussions),
raise an issue, or open a pull request.

## Request For Comments

For most of the changes, the normal pull request workflow is sufficient. However, some significant changes
should go through a design process and consensus must be reached among maintainers. For this purpose, we have
the **Request For Comments** process. RFC should be drafted for example when:

* introducing a new feature or an API,
* redesigning internals in a major way,
* or considering breaking changes.

When drafting an RFC, follow the [template](proposals/000-rfc-template.md) and create a pull request to start the discussion.
The pull request should be tagged with the label `proposal`.

## Development prerequisites

* [Go](https://golang.org/) >= 1.22
* [Protocol Buffer compiler](https://grpc.io/docs/protoc-installation/) >= 3
* [Go Protocol Buffer compiler plugin](https://pkg.go.dev/github.com/golang/protobuf/protoc-gen-go)
  -- `go install google.golang.org/protobuf/cmd/protoc-gen-go`
* [Go Vitess Protocol Buffers compiler](https://github.com/planetscale/vtprotobuf/)
  -- `go install github.com/planetscale/vtprotobuf/cmd/protoc-gen-go-vtproto`
* [Go gRPC generator Protocol Buffer compiler plugin](https://pkg.go.dev/google.golang.org/grpc/cmd/protoc-gen-go-grpc)
  -- `go install google.golang.org/grpc/cmd/protoc-gen-go-grpc`
* [Go documentation Protocol Buffer compiler plugin](https://github.com/pseudomuto/protoc-gen-doc)
  -- `go install github.com/pseudomuto/protoc-gen-doc/cmd/protoc-gen-doc`

## Testing prerequisites

* [Docker](https://www.docker.com) and [kind](https://kind.sigs.k8s.io/) — used for manual integration testing
* [gRPC curl](https://github.com/fullstorydev/grpcurl) — useful for testing the gRPC API interactively

## Development Workflow

### Build

```bash
# Build only the armada binary (fast iteration)
make armada

# Build only the control CLI binary
make arctl

# Regenerate protobuf outputs and build the binaries (full build)
make build

# Regenerate protobuf outputs only
make proto
```

### Test

```bash
# Run the full test suite with race detection and coverage
make test

# Run tests for a single package
go test ./storage/table/...

# Run a single named test
go test ./storage/table/... -run '^TestTableManager$'
```

### Lint

```bash
# Run the linter (runs make proto first)
make check
```

### Run locally

```bash
# Start a single-node leader cluster in development mode
make run

# Start a follower connected to the local leader (requires make run to be running)
make run-follower
```

## Running Documentation Site Locally

This documentation site is powered by [Jekyll](https://jekyllrb.com).
First, install Jekyll and [bundler](https://bundler.io):

```bash
gem install bundler jekyll
```

Then, install the necessary gems:

```bash
cd ./docs
bundle install
```

Last, run Jekyll in the root of the repository:

```bash
make serve-docs
```

## Useful links

* [gRPC in Golang](https://grpc.io/docs/languages/go/)
* [Protobuffers in JSON](https://developers.google.com/protocol-buffers/docs/proto3#json)
* [Dragonboat](https://github.com/lni/dragonboat)
* [Raft algorithm](https://raft.github.io)
* [Pebble](https://github.com/cockroachdb/pebble)
