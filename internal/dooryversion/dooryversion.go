// Package dooryversion is the single pin on the doory upstream being ported.
// Nothing else in the tree may embed the upstream commit; the mirror test
// (test/upstream-parity/mirror_version_test.go) checks .upstream/current
// against these values.
package dooryversion

const (
	// UpstreamRepo is the git remote of the behavior oracle.
	UpstreamRepo = "https://github.com/shaongitbd/doory"

	// UpstreamCommit is the sole pinned oracle commit. Every PORT_MAP row,
	// ledger, and parity scenario describes behavior at this commit.
	UpstreamCommit = "2eb2557c1eed3aa256a78f1874abfb5a1315849c"

	// UpstreamReleaseLine records the human-readable describe of the pin.
	UpstreamReleaseLine = "doory@2eb2557 (2026-10-20, High-contrast mode (#120))"

	// Version is this port's release line: <port-version>+<upstream-short>.
	Version = "0.0.1+" + "2eb2557"
)
