// Unit evidence for the element pack sources and readers: the glob matcher,
// safe archive paths, tarball reading (npm shape), directory reading, the
// Iconify JSON reader and the manifest's own patterns. The pipeline above them
// is end-to-end covered by scenario 22-ops-commands (generated packs) and by
// the normaliser conformance fixtures.
package doory

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func gzipTarball(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)
	for name, content := range entries {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := archive.Write([]byte(content)); err != nil {
			t.Fatalf("tar write: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func TestGlobMatcherSemantics(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"icons/*.svg", "icons/a.svg", true},
		{"icons/*.svg", "icons/nested/a.svg", false}, // * never crosses /
		{"icons/**/*.svg", "icons/nested/a.svg", true},
		{"icons/**", "icons/nested/a.svg", true},
		{"*.svg", "package.json", false},
		{"?.svg", "a.svg", true},
		{"?.svg", "ab.svg", false},
		{"icons/a.svg", "icons/a.svg", true},
		{"icons/a.svg", "icons/A.svg", false},
	}
	for _, tc := range cases {
		match := matcher([]string{tc.pattern})
		if got := match(tc.path); got != tc.want {
			t.Errorf("match(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestArchivePathRefusesUnsafeNames(t *testing.T) {
	safe := map[string]string{
		"package/icons/a.svg": "icons/a.svg",
		"pkg\\icons\\b.svg":   "icons/b.svg", // #nosec G101 -- not a credential
	}
	for input, want := range safe {
		got, ok := archivePath(input)
		if !ok || got != want {
			t.Errorf("archivePath(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
	// Python's posixpath.normpath collapses double slashes, so this one is safe.
	if got, ok := archivePath("package//a.svg"); !ok || got != "a.svg" {
		t.Errorf("archivePath(package//a.svg) = %q, %v; want a.svg", got, ok)
	}
	for _, input := range []string{"a.svg", "/abs/a.svg", "package/../a.svg"} {
		if got, ok := archivePath(input); ok {
			t.Errorf("archivePath(%q) = %q, want refusal", input, got)
		}
	}
}

func TestReadTarballSelectsPatterns(t *testing.T) {
	data := gzipTarball(t, map[string]string{
		"package/icons/a.svg":        "<svg/>",
		"package/icons/b.svg":        "<svg/>",
		"package/LICENSE":            "MIT",
		"package/package.json":       "{}",
		"package/icons/nested/c.svg": "<svg/>",
	})
	found, err := readTarball(data, []string{"icons/*.svg", "LICENSE"})
	if err != nil {
		t.Fatalf("readTarball: %v", err)
	}
	want := map[string]string{"icons/a.svg": "<svg/>", "icons/b.svg": "<svg/>", "LICENSE": "MIT"}
	if !reflect.DeepEqual(found, map[string][]byte{"icons/a.svg": []byte("<svg/>"),
		"icons/b.svg": []byte("<svg/>"), "LICENSE": []byte("MIT")}) {
		t.Fatalf("readTarball = %v, want %v", found, want)
	}
}

func TestReadPackDirectoryAndUrlIntegrity(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "icons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "icons", "a.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := readPackDirectory(dir, []string{"icons/*.svg"})
	if err != nil || len(found) != 1 {
		t.Fatalf("readPackDirectory = %v, %v", found, err)
	}
	// The urls source verifies the pinned per-file SHA-256.
	spec := urlsSource{Index: "packs.json", Files: []urlFile{
		{Name: "a.svg", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"},
	}}
	source := directoryPackSource{dir: dir}
	// The index folder is packs/ (index minus .json) and holds the files.
	if err := os.MkdirAll(filepath.Join(dir, "packs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "packs", "a.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := source.fetch(spec, nil); err == nil {
		t.Fatal("a mismatched SHA-256 must be refused")
	}
}

func TestReadIconifyJSONExpandsIcons(t *testing.T) {
	raw := []byte(`{"info":{},"prefix":"test","icons":{"home":{"body":"<path d=\"M0 0\"/>"},
	  "house":{"body":"<path d=\"M1 1\"/>","hidden":true},"arrow":{"body":"<path d=\"M2 2\"/>","width":24}},
	  "aliases":{"index":{"parent":"home"}},"width":16,"height":16}`)
	icons, err := readIconifyJSON(raw)
	if err != nil {
		t.Fatalf("readIconifyJSON: %v", err)
	}
	if len(icons) != 2 || icons[0].Name != "arrow" || icons[1].Name != "home" {
		t.Fatalf("icons = %+v", icons)
	}
	if got := icons[0].AliasesAsWords(); len(got) != 0 {
		t.Fatalf("arrow aliases = %v", got)
	}
	if got := icons[1].AliasesAsWords(); !reflect.DeepEqual(got, []string{"index"}) {
		t.Fatalf("home aliases = %v", got)
	}
	if string(icons[0].SVG) != `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 24 16"><path d="M2 2"/></svg>` {
		t.Fatalf("arrow svg = %s", icons[0].SVG)
	}
}

func TestManifestPatternsMatchRealPaths(t *testing.T) {
	// The lucide pack's include pattern must match an npm tarball's icon path
	// (paths are archive-relative, top folder dropped).
	match := matcher([]string{"icons/*.svg"})
	for _, path := range []string{"icons/ab-test.svg", "icons/zap.svg"} {
		if !match(path) {
			t.Fatalf("lucide pattern misses %q", path)
		}
	}
	if match("icons/ab-test.json") {
		t.Fatal("lucide pattern must not match JSON")
	}
}
