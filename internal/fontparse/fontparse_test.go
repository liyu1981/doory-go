// PROBE-ANCHORED cross-implementation evidence: the pinned oracle's own
// probe_worker.py produced these exact facts for the fixtures
// (probes/0006-fonts + fixture metadata probe run). Mutation: any table
// offset, weight/style rule, cmap coverage or name preference change makes
// these fail while a Go-only round trip could still pass.
package fontparse

import (
	"os"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../test/parity/testdata/fonts/" + name)
	if err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	return b
}

var wantBassaVah = Info{
	Family: "Noto Sans Bassa Vah", Subfamily: "Regular", Format: "ttf",
	Weight: 400, Italic: false, IsVariable: false,
	Scripts:  nil, // oracle probe: [] (covers no probe script)
	Category: "sans-serif", Glyphs: 45,
}

func TestInspectTTFProbeFacts(t *testing.T) {
	got, err := Inspect(loadFixture(t, "fixture.ttf"))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	want := wantBassaVah
	if got.Family != want.Family || got.Subfamily != want.Subfamily || got.Weight != want.Weight ||
		got.Italic != want.Italic || got.IsVariable != want.IsVariable || got.Category != want.Category ||
		got.Glyphs != want.Glyphs || len(got.Scripts) != 0 {
		t.Errorf("got %+v, want %+v", *got, want)
	}
}

// The WOFF2 path must reach the SAME facts as the TTF (oracle sees identical
// metadata through fontTools' brotli+transform decoding).
func TestInspectWOFF2MatchesTTF(t *testing.T) {
	got, err := Inspect(loadFixture(t, "fixture.woff2"))
	if err != nil {
		t.Fatalf("inspect woff2: %v", err)
	}
	want := wantBassaVah
	want.Format = WOFF2
	if got.Family != want.Family || got.Weight != want.Weight || got.Category != want.Category ||
		got.Glyphs != want.Glyphs || len(got.Scripts) != 0 {
		t.Errorf("woff2 got %+v, want %+v", *got, want)
	}
}

func TestInspectSecondFamily(t *testing.T) {
	got, err := Inspect(loadFixture(t, "fixture2.ttf"))
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if got.Family != "Noto Sans Buginese" {
		t.Errorf("family = %q, want Noto Sans Buginese", got.Family)
	}
}

func TestSniffRejects(t *testing.T) {
	if _, err := Inspect([]byte("not a font at all")); err == nil ||
		err.Error() != "The file is not a TTF, OTF, WOFF or WOFF2 font." {
		t.Errorf("junk: %v", err)
	}
	if _, err := Inspect([]byte("ttcf........")); err == nil ||
		err.Error() != "Font collections (.ttc/.otc) are not supported. Upload the individual fonts." {
		t.Errorf("ttc: %v", err)
	}
}
