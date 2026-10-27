package doory

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRunInstallServiceWritesBinaryConfigAndUnit(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "doory-source")
	if err := os.WriteFile(source, []byte("fake-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	opts := installServiceOptions{
		port:       9000,
		home:       home,
		configHome: filepath.Join(home, ".config"),
		executable: source,
		systemctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
	}
	var stdout, stderr bytes.Buffer
	if err := runInstallService(opts, &stdout, &stderr); err != nil {
		t.Fatalf("install: %v\n%s", err, stderr.String())
	}

	target := filepath.Join(home, ".local", "bin", "doory")
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if string(installed) != "fake-binary" {
		t.Errorf("installed binary = %q", installed)
	}
	if info, err := os.Stat(target); err != nil {
		t.Errorf("stat installed binary: %v", err)
	} else if info.Mode().Perm() != 0o755 {
		t.Errorf("installed binary mode = %v, want 0755", info.Mode().Perm())
	}

	configFile := filepath.Join(home, ".config", "doory", ".env")
	config, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	for _, want := range []string{"template-version:", "SITE_URL=http://localhost:9000", "DJANGO_ALLOWED_HOSTS="} {
		if !strings.Contains(string(config), want) {
			t.Errorf("config missing %q", want)
		}
	}
	if info, err := os.Stat(configFile); err != nil {
		t.Errorf("stat config: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %v, want 0600", info.Mode().Perm())
	}

	unitFile := filepath.Join(home, ".config", "systemd", "user", "doory.service")
	unit, err := os.ReadFile(unitFile)
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	for _, want := range []string{
		"WorkingDirectory=%h/.doory",
		"Environment=HEALTH_PORT=9000",
		"ExecStart=%h/.local/bin/doory entrypoint :9000",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("unit missing %q", want)
		}
	}
	if strings.Contains(string(unit), "EnvironmentFile=") {
		t.Error("unit still uses EnvironmentFile; the binary reads ~/.config/doory/.env itself")
	}

	wantCalls := [][]string{{"daemon-reload"}, {"enable", "--now", "doory.service"}, {"restart", "doory.service"}}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("systemctl calls = %v, want %v", calls, wantCalls)
	}
	if !strings.Contains(stdout.String(), "config:  "+configFile) {
		t.Errorf("output does not name the config file: %q", stdout.String())
	}
}

func TestRunInstallServiceMigratesLegacyEnv(t *testing.T) {
	home := t.TempDir()
	legacyDir := filepath.Join(home, ".doory")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(legacyDir, "doory.env")
	legacyBody := "DJANGO_SECRET_KEY=old-secret\nSITE_URL=https://old.example.test\n"
	if err := os.WriteFile(legacy, []byte(legacyBody), 0o600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "doory-source")
	if err := os.WriteFile(source, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	opts := installServiceOptions{
		port:       8111,
		home:       home,
		configHome: filepath.Join(home, ".config"),
		executable: source,
		systemctl:  func(args ...string) error { return nil },
	}
	var stdout, stderr bytes.Buffer
	if err := runInstallService(opts, &stdout, &stderr); err != nil {
		t.Fatalf("install: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "migrated") {
		t.Errorf("migration not reported: %q", stdout.String())
	}
	config, err := os.ReadFile(filepath.Join(home, ".config", "doory", ".env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"DJANGO_SECRET_KEY=old-secret",
		"SITE_URL=https://old.example.test",
		"template-version:",
	} {
		if !strings.Contains(string(config), want) {
			t.Errorf("migrated config missing %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "doory", ".env.bak")); err != nil {
		t.Errorf("upgrade did not keep a backup while migrating: %v", err)
	}
}

func TestRunInstallServiceUninstallKeepsStateAndConfig(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	unitFile := filepath.Join(configHome, "systemd", "user", "doory.service")
	configFile := filepath.Join(configHome, "doory", ".env")
	dataFile := filepath.Join(home, ".doory", "data", "doory.sqlite3")
	for _, file := range []string{unitFile, configFile, dataFile} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var calls [][]string
	opts := installServiceOptions{
		uninstall:  true,
		home:       home,
		configHome: configHome,
		systemctl: func(args ...string) error {
			calls = append(calls, append([]string(nil), args...))
			return nil
		},
	}
	var stdout, stderr bytes.Buffer
	if err := runInstallService(opts, &stdout, &stderr); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(unitFile); !os.IsNotExist(err) {
		t.Errorf("unit still exists after uninstall: %v", err)
	}
	for _, file := range []string{configFile, dataFile} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("uninstall removed %s: %v", file, err)
		}
	}
	wantCalls := [][]string{{"disable", "--now", "doory.service"}, {"daemon-reload"}}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Errorf("systemctl calls = %v, want %v", calls, wantCalls)
	}
}

func TestRunInstallServiceCommandHelpAndPortValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := RunInstallServiceCommand([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: doory install-service") {
		t.Errorf("help output = %q", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunInstallServiceCommand([]string{"--port", "70000"}, &stdout, &stderr); code != 2 {
		t.Fatalf("invalid port exit = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid port") {
		t.Errorf("invalid port stderr = %q", stderr.String())
	}
}
