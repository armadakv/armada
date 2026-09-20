package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The image the harness builds and runs. Fixed rather than per-run so that the
// Docker layer cache is shared across runs.
const (
	imageRepo = "armada-integration"
	imageTag  = "latest"
)

// Build the image at most once per `go test` process. Every scenario shares it.
var (
	imageOnce sync.Once
	imageRef  string
	imageErr  error
)

// armadaImage returns a runnable Armada image reference, building it from the
// working tree's Dockerfile on first use.
//
// The build is not skipped when the image already exists: Docker's layer cache
// keys `COPY . .` on content, so an unchanged tree is close to free while an
// edited one is picked up automatically. A harness that silently tested stale
// binaries would be worse than no harness at all.
//
// It shells out to `docker build` rather than using the Docker API directly
// because the Dockerfile mounts the Go module and build caches
// (`RUN --mount=type=cache`), which only BuildKit understands. The classic
// builder that the API exposes rejects those outright, and dropping them would
// turn every rebuild into a full recompile of the dependency tree.
func armadaImage(ctx context.Context, t *testing.T) string {
	t.Helper()
	imageOnce.Do(func() {
		if ref := os.Getenv("ARMADA_TEST_IMAGE"); ref != "" {
			infof("using prebuilt image %s (ARMADA_TEST_IMAGE)", ref)
			imageRef = ref
			return
		}
		root, err := repoRoot()
		if err != nil {
			imageErr = err
			return
		}
		ref := imageRepo + ":" + imageTag
		infof("building %s from %s (uncached first build takes a few minutes)", ref, root)
		started := time.Now()
		if err := dockerBuild(ctx, root, ref); err != nil {
			imageErr = err
			return
		}
		infof("built %s in %s", ref, time.Since(started).Round(time.Second))
		imageRef = ref
	})
	if imageErr != nil {
		t.Fatalf("cannot obtain armada image: %v", imageErr)
	}
	return imageRef
}

func dockerBuild(ctx context.Context, root, ref string) error {
	bin, err := exec.LookPath("docker")
	if err != nil {
		return fmt.Errorf("the harness builds its image with the docker CLI, which is not on PATH: %w\n"+
			"either install it or point the harness at a prebuilt image with ARMADA_TEST_IMAGE", err)
	}
	cmd := exec.CommandContext(ctx, bin, "build",
		"--build-arg", "VERSION=integration",
		"-f", filepath.Join(root, "Dockerfile"),
		"-t", ref,
		root,
	)
	// Buffered rather than streamed: a successful build is noise, and a failed
	// one needs the whole log, not the tail that scrolled past.
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	// Docker 23+ defaults to BuildKit, but an older CLI or an explicit
	// DOCKER_BUILDKIT=0 in the environment would silently fall back to the
	// classic builder and fail on the cache mounts.
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build: %w\n--- docker build output ---\n%s", err, out.String())
	}
	return nil
}

// repoRoot walks up from the working directory to the root of the main Armada
// module, which is the Docker build context and the source of the replication
// TLS fixtures.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.Contains(b, []byte("module github.com/armadakv/armada\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("could not find the armada module root above " + dir)
		}
		dir = parent
	}
}
