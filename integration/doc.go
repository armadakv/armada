// Package integration holds the container-based end-to-end harness for
// inter-cluster snapshot recovery (RFC 005, docs/proposals/005-*.md).
//
// It brings up two real three-node Armada clusters in Docker — a leader cluster
// exporting snapshot artefacts to a shared filesystem store, and a follower
// cluster recovering from them — and asserts the recovery paths end to end.
//
// This is a separate Go module so that its container-runtime dependencies never
// enter the main module's graph; nested modules are invisible to the repository
// root's `go build ./...` and `go test ./...`, so nothing here runs unless it is
// asked for by name. Run it with:
//
//	make test-integration
//	make test-integration-one SCENARIO=full
//	cd integration && go test -count=1 -timeout=60m ./...
//
// See README.md for usage and configuration.
package integration
