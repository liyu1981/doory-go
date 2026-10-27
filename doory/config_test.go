package doory

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunConfigCommandInitAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := RunConfigCommand([]string{"init", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr.String())
	}
	path := filepath.Join(dir, ".env")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "template-version:") {
		t.Fatal("init did not write the template")
	}

	configured := strings.Replace(string(data), "SITE_URL=http://localhost:8080", "SITE_URL=https://doory.example.com", 1)
	configured = strings.Replace(configured, "template-version: 1", "template-version: 0", 1)
	if err := os.WriteFile(path, []byte(configured), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunConfigCommand([]string{"upgrade", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("upgrade exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "upgraded") {
		t.Errorf("upgrade output = %q", stdout.String())
	}
	upgraded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upgraded), "SITE_URL=https://doory.example.com") {
		t.Error("upgrade lost the configured SITE_URL")
	}

	stdout.Reset()
	stderr.Reset()
	if code := RunConfigCommand([]string{"upgrade", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("second upgrade exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Errorf("second upgrade output = %q", stdout.String())
	}
}

func TestRunConfigCommandRejectsOverwrite(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := RunConfigCommand([]string{"init", dir}, &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunConfigCommand([]string{"init", dir}, &stdout, &stderr); code != 1 {
		t.Fatalf("second init exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
