package integration

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tclog "github.com/testcontainers/testcontainers-go/log"
)

// The harness writes its own progress, and testcontainers' container
// bookkeeping, in the same layout the armada processes use:
//
//	2026-09-16T11:46:54.448Z<TAB>INFO<TAB>harness<TAB>follower converged to 2000 keys
//	2026-09-16T11:46:54.451Z<TAB>INFO<TAB>docker<TAB>🐳 Starting container: d765c5ca6cf0
//	2026-09-16T11:46:54.448Z<TAB>INFO<TAB>manager<TAB>table/recovery.go:680<TAB>[armada-test] …
//
// The third line is a real armada log line out of a saved container log. Having
// all three share a shape means one `sort` merges a run's harness output with
// the six nodes' logs into a single readable timeline, which is the only
// practical way to work out what a distributed recovery actually did.
const logTimeFormat = "2006-01-02T15:04:05.000Z"

// active is the test whose log the harness is currently writing to.
//
// It is a package-level variable because testcontainers' logger is set
// globally, once, and has no way to carry a *testing.T. Scenarios never run in
// parallel — they share one pair of clusters — so a single slot is enough.
var active atomic.Pointer[testing.T]

func init() {
	tclog.SetDefault(namedLogger("docker"))
}

// setActive redirects harness logging at t and returns a function restoring the
// previous target.
func setActive(t *testing.T) func() {
	prev := active.Swap(t)
	return func() { active.Store(prev) }
}

// infof writes one harness progress line.
//
// Informational output goes here rather than to t.Logf so that it is not
// prefixed with the harness's own file and line, which says nothing useful.
// Failures stay on t.Errorf/t.Fatalf, where that prefix does point at the
// assertion that fired.
func infof(format string, args ...any) {
	writeLog("harness", format, args...)
}

// namedLogger adapts the harness format to testcontainers' Logger interface.
type namedLogger string

func (n namedLogger) Printf(format string, v ...any) { writeLog(string(n), format, v...) }

func writeLog(name, format string, args ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	line := fmt.Sprintf("%s\tINFO\t%s\t%s\n", time.Now().UTC().Format(logTimeFormat), name, msg)
	fmt.Fprint(logWriter(), line)
}

// logWriter is the running test's log, or stderr when there is no test — which
// happens for anything testcontainers logs from its own background goroutines
// after a test has returned.
func logWriter() io.Writer {
	if t := active.Load(); t != nil {
		return t.Output()
	}
	return os.Stderr
}
