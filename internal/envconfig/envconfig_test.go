package envconfig

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTemplateMatchesRootExample(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != Template {
		t.Fatal(".env.example and internal/envconfig/env.example differ; copy the root file into the package")
	}
	if TemplateVersion() < 1 {
		t.Fatalf("template version = %d, want >= 1", TemplateVersion())
	}
}

func TestParseEnv(t *testing.T) {
	data := []byte(`
# comment
SITE_URL=http://localhost:8080
export EXPORTED=yes
QUOTED="hello world"
SINGLE='literal # not a comment'
ESCAPED="line\nbreak"
INLINE=value # trailing comment
HASH=value#not-a-comment
EMPTY=
`)
	values := parseEnv(data)
	want := map[string]string{
		"SITE_URL": "http://localhost:8080",
		"EXPORTED": "yes",
		"QUOTED":   "hello world",
		"SINGLE":   "literal # not a comment",
		"ESCAPED":  "line\nbreak",
		"INLINE":   "value",
		"HASH":     "value#not-a-comment",
		"EMPTY":    "",
	}
	for key, wantValue := range want {
		if got := values[key]; got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
	if len(values) != len(want) {
		t.Errorf("parsed %d values, want %d: %v", len(values), len(want), values)
	}
}

func TestLoadFillsOnlyAbsentVariables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("DOORY_TEST_PRESENT=from-file\nDOORY_TEST_ABSENT=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOORY_TEST_PRESENT", "from-process")
	t.Cleanup(func() { _ = os.Unsetenv("DOORY_TEST_ABSENT") })

	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DOORY_TEST_PRESENT"); got != "from-process" {
		t.Errorf("process value overridden: got %q", got)
	}
	if got := os.Getenv("DOORY_TEST_ABSENT"); got != "from-file" {
		t.Errorf("missing variable not filled: got %q", got)
	}
}

func TestLoadMissingDefaultIsIgnoredButExplicitIsNot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if path, err := Load(""); err != nil || path != "" {
		t.Fatalf("missing default: path=%q err=%v, want empty/nil", path, err)
	}
	missing := filepath.Join(t.TempDir(), "missing.env")
	if _, err := Load(missing); err == nil {
		t.Fatal("explicit missing env file did not fail")
	}
}

func TestInitRefusesAndForceBacksUp(t *testing.T) {
	dir := t.TempDir()
	path, err := Init(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != Template {
		t.Fatalf("init wrote wrong content (err=%v)", err)
	}
	if _, err := Init(dir, false); err == nil {
		t.Fatal("second init without --force did not fail")
	}
	if _, err := Init(dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("force init did not keep a backup: %v", err)
	}
}

func TestUpgradePreservesValuesAndAddsTemplateKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	old := strings.Join([]string{
		"# my old file",
		"# template-version: 0",
		"SITE_URL=https://doory.example.com",
		"DJANGO_SECRET_KEY=super-secret",
		"CUSTOM_ONLY=keep-me",
		"# OLD_COMMENTED=old",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Upgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed {
		t.Fatal("upgrade reported no change")
	}
	if result.OldVersion != 0 || result.NewVersion != TemplateVersion() {
		t.Fatalf("versions = %d -> %d", result.OldVersion, result.NewVersion)
	}
	if result.BackupPath == "" {
		t.Fatal("no backup path")
	}
	if _, err := os.Stat(result.BackupPath); err != nil {
		t.Fatalf("backup missing: %v", err)
	}

	merged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	for _, want := range []string{
		"SITE_URL=https://doory.example.com",
		"DJANGO_SECRET_KEY=super-secret",
		"CUSTOM_ONLY=keep-me",
		"# template-version: " + strconv.Itoa(TemplateVersion()),
		"# REALTIME_ENABLED=true",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("upgraded file missing %q", want)
		}
	}
	if strings.Contains(text, "OLD_COMMENTED=old") {
		t.Error("commented setting leaked into the upgraded file")
	}
	values := parseEnv(merged)
	if values["SITE_URL"] != "https://doory.example.com" || values["DJANGO_SECRET_KEY"] != "super-secret" {
		t.Errorf("configured values not preserved: %v", values)
	}
	if values["CUSTOM_ONLY"] != "keep-me" {
		t.Error("custom key not preserved")
	}

	// The second run is a no-op.
	second, err := Upgrade(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed {
		t.Errorf("upgrade is not idempotent: added=%v documented=%v", second.Added, second.Documented)
	}
}

func TestVersionOf(t *testing.T) {
	if got := versionOf([]byte("# template-version: 7\n")); got != 7 {
		t.Errorf("version = %d, want 7", got)
	}
	if got := versionOf([]byte("SITE_URL=x\n")); got != 0 {
		t.Errorf("version = %d, want 0", got)
	}
}

func TestLoadDefaultXDGPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	if err := os.MkdirAll(filepath.Join(base, "doory"), 0o700); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(base, "doory", ".env")
	if err := os.WriteFile(wantPath, []byte("DOORY_TEST_XDG=loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("DOORY_TEST_XDG") })
	path, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if got := os.Getenv("DOORY_TEST_XDG"); got != "loaded" {
		t.Errorf("DOORY_TEST_XDG = %q, want loaded", got)
	}
}
