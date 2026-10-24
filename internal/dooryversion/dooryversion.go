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
	UpstreamCommit = "2fb4d0297ec4c6e417b5225215502f8217c2a4dc"

	// UpstreamReleaseLine records the human-readable describe of the pin.
	UpstreamReleaseLine = "doory@v0.2.0 (2fb4d02, 2026-10-23)"

	// Version is this port's release line: <port-version>+<upstream-short>.
	Version = "0.2.0+" + "2fb4d02"
)
