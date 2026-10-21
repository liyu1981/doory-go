// Package upstreamparity rejects a .upstream/current mirror that does not
// match internal/dooryversion. The mirror is the behavior oracle; a mismatched
// mirror would silently move every parity claim.
package upstreamparity

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"doory-go/internal/dooryversion"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine test file location")
	}
	// test/upstream-parity -> repo root
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestMirrorMatchesPin(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", filepath.Join(root, ".upstream", "current"), "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("reading mirror HEAD: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != dooryversion.UpstreamCommit {
		t.Fatalf("mirror HEAD %s does not match pin %s; run `make upstream-mirror`", got, dooryversion.UpstreamCommit)
	}
	// The mirror must be pristine: any local modification invalidates every probe.
	status, err := exec.Command("git", "-C", filepath.Join(root, ".upstream", "current"), "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("reading mirror status: %v", err)
	}
	if s := strings.TrimSpace(string(status)); s != "" {
		t.Fatalf("mirror has local modifications (first lines):\n%s", s)
	}
}
