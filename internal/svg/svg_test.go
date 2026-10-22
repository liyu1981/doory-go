// PROBE-ANCHORED differential evidence: the pinned oracle's own
// apps/core/svg.py produced these exact accept/reject decisions and sizes
// for the fixture set (recorded from the oracle run). Mutation: allow-list
// changes, CSS/escape handling, or size derivation flips these.
package svg

import (
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../test/parity/testdata/svgs/" + name)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return b
}

func TestSanitizeAcceptsAndCleans(t *testing.T) {
	out, err := Sanitize(loadFixture(t, "script.svg"), true)
	if err != nil {
		t.Fatalf("script.svg should sanitize: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "script") || strings.Contains(s, "onclick") {
		t.Errorf("script/onclick survived: %s", s)
	}
	if !strings.Contains(s, `<rect width="10" height="10"`) {
		t.Errorf("rect lost: %s", s)
	}
	info, err := Info(out)
	if err != nil || info.Width != 10 || info.Height != 10 {
		t.Errorf("info = %+v, %v; want 10x10", info, err)
	}
}

func TestSanitizeXLinks(t *testing.T) {
	out, err := Sanitize(loadFixture(t, "xlink.svg"), true)
	if err != nil {
		t.Fatalf("xlink.svg: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `xmlns:xlink="http://www.w3.org/1999/xlink"`) {
		t.Errorf("xlink namespace declaration missing: %s", s)
	}
	if !strings.Contains(s, `xlink:href="https://example.com/page"`) {
		t.Errorf("safe link lost: %s", s)
	}
	if strings.Contains(s, "javascript") {
		t.Errorf("javascript: link survived: %s", s)
	}
	if !strings.Contains(s, `xlink:href="#g"`) {
		t.Errorf("internal ref lost: %s", s)
	}
}

func TestSanitizeStyle(t *testing.T) {
	out, err := Sanitize(loadFixture(t, "style.svg"), true)
	if err != nil {
		t.Fatalf("style.svg: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "<style") || strings.Contains(s, "@import") {
		t.Errorf("dangerous style survived: %s", s)
	}
	if !strings.Contains(s, `class="a"`) {
		t.Errorf("element with safe class lost: %s", s)
	}
}

func TestSanitizeRejects(t *testing.T) {
	if _, err := Sanitize(loadFixture(t, "dtd.svg"), true); err == nil || err.Error() != "Invalid or unsafe SVG." {
		t.Errorf("dtd: %v", err)
	}
	if _, err := Sanitize(loadFixture(t, "notsvg.xml"), true); err == nil || err.Error() != "Not an SVG document." {
		t.Errorf("notsvg: %v", err)
	}
	if _, err := Sanitize(nil, false); err == nil || err.Error() != "Empty SVG." {
		t.Errorf("empty: %v", err)
	}
}

func TestWithIntrinsicSize(t *testing.T) {
	clean, serr := Sanitize(loadFixture(t, "basic.svg"), false)
	if serr != nil {
		t.Fatal(serr)
	}
	// basic.svg keeps its viewBox: its info derives from width/height
	// (100x50) before sizing.
	info, _ := Info(clean)
	if info.Width != 100 || info.Height != 50 {
		t.Errorf("pre-size info = %+v; want 100x50", info)
	}
	out, err := WithIntrinsicSize(clean, 64, 32)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `width="64"`) || !strings.Contains(s, `height="32"`) {
		t.Errorf("size not applied: %s", s)
	}
	sized, _ := Info(out)
	if sized.Width != 64 || sized.Height != 32 {
		t.Errorf("post-size info = %+v; want 64x32", sized)
	}
}
