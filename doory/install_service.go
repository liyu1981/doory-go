// The built-in `doory install-service` command: install, configure and manage
// the systemd USER unit. It is the Go replacement for the old
// scripts/install-systemd.sh (the script remains as a thin build-and-delegate
// wrapper). No root is needed: the binary goes to ~/.local/bin, the unit to
// ~/.config/systemd/user, the configuration to ~/.config/doory/.env and the
// state to ~/.doory.
package doory

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"doory-go/internal/envconfig"
)

const installServiceUnit = "doory.service"

// installServiceOptions is the injectable core of the command (tests use a
// temporary home and a fake systemctl).
type installServiceOptions struct {
	port       int
	uninstall  bool
	home       string
	configHome string
	executable string
	systemctl  func(args ...string) error
}

// RunInstallServiceCommand dispatches `doory install-service`; returns the
// process exit code.
func RunInstallServiceCommand(argv []string, stdout, stderr io.Writer) int {
	for _, arg := range argv {
		if arg == "-h" || arg == "--help" {
			printInstallServiceHelp(stdout)
			return 0
		}
	}
	args, err := parseArgs(argv, []argSpec{
		{name: "port", kind: "int"},
		{name: "uninstall", kind: "bool"},
	})
	if err != nil {
		return installServiceUsageError(err, stderr)
	}
	if len(args.positionals) > 0 {
		return installServiceUsageError(&usageError{
			message: "unrecognized arguments: " + strings.Join(args.positionals, " "),
		}, stderr)
	}
	port := args.intOption("port", 8111)
	if port < 1 || port > 65535 {
		return installServiceUsageError(&usageError{
			message: fmt.Sprintf("argument --port: invalid port: %d", port),
		}, stderr)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintln(stderr, "doory install-service: cannot determine the home directory")
		return 1
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	} else {
		configHome = envconfig.ExpandPath(configHome)
	}
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "doory install-service: cannot locate the running binary: %v\n", err)
		return 1
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	opts := installServiceOptions{
		port:       port,
		uninstall:  args.bool("uninstall"),
		home:       home,
		configHome: configHome,
		executable: executable,
		systemctl:  runUserSystemctl,
	}
	if err := runInstallService(opts, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "doory install-service: %v\n", err)
		return 1
	}
	return 0
}

func printInstallServiceHelp(w io.Writer) {
	fmt.Fprintln(w, "usage: doory install-service [-h] [--port PORT] [--uninstall]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Install Doory as a systemd USER service (no root needed):")
	fmt.Fprintln(w, "  binary:  ~/.local/bin/doory")
	fmt.Fprintln(w, "  unit:    ~/.config/systemd/user/doory.service")
	fmt.Fprintln(w, "  config:  ~/.config/doory/.env")
	fmt.Fprintln(w, "  state:   ~/.doory/{data,logs}")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --port PORT   listen address (default 8111)")
	fmt.Fprintln(w, "  --uninstall   stop, disable and remove the unit (keeps state and config)")
	fmt.Fprintln(w, "  -h, --help    show this help")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The command copies the running binary, writes/upgrades the .env from the")
	fmt.Fprintln(w, "embedded template (migrating an old ~/.doory/doory.env once), writes the")
	fmt.Fprintln(w, "systemd unit and runs 'systemctl --user enable --now doory.service'")
	fmt.Fprintln(w, "followed by a restart, so a re-run switches to the new binary.")
}

func installServiceUsageError(err error, stderr io.Writer) int {
	message := err.Error()
	if usageErr, ok := err.(*usageError); ok {
		message = usageErr.message
	}
	fmt.Fprintln(stderr, "usage: doory install-service [-h] [--port PORT] [--uninstall]")
	fmt.Fprintf(stderr, "doory install-service: error: %s\n", message)
	return 2
}

// runInstallService performs the install or uninstall with paths already
// resolved (the command wires the real home, executable and systemctl).
func runInstallService(opts installServiceOptions, stdout, stderr io.Writer) error {
	dataDir := filepath.Join(opts.home, ".doory")
	binDir := filepath.Join(opts.home, ".local", "bin")
	targetBin := filepath.Join(binDir, "doory")
	configDir := filepath.Join(opts.configHome, "doory")
	configFile := filepath.Join(configDir, ".env")
	legacyEnv := filepath.Join(dataDir, "doory.env")
	unitDir := filepath.Join(opts.configHome, "systemd", "user")
	unitFile := filepath.Join(unitDir, installServiceUnit)

	if opts.uninstall {
		// Best effort, exactly like the shell script: an already-stopped or
		// already-removed unit must not make uninstall fail.
		_ = opts.systemctl("disable", "--now", installServiceUnit)
		if err := os.Remove(unitFile); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", unitFile, err)
		}
		_ = opts.systemctl("daemon-reload")
		fmt.Fprintf(stdout, "install-service: unit removed (state in %s and config in %s are untouched)\n", dataDir, configDir)
		return nil
	}

	// 1. Install the running binary.
	if err := installExecutable(opts.executable, targetBin); err != nil {
		return err
	}

	// 2. State, logs and configuration.
	for _, dir := range []string{dataDir, filepath.Join(dataDir, "data"), filepath.Join(dataDir, "logs"), configDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if _, legacyErr := os.Stat(legacyEnv); legacyErr == nil {
			if err := copyFileMode(legacyEnv, configFile, 0o600); err != nil {
				return fmt.Errorf("migrate %s: %w", legacyEnv, err)
			}
			fmt.Fprintf(stdout, "install-service: migrated %s -> %s\n", legacyEnv, configFile)
		} else {
			if _, err := envconfig.Init(configDir, false); err != nil {
				return err
			}
		}
	} else if err != nil {
		return fmt.Errorf("check %s: %w", configFile, err)
	}
	// Bring the file up to the current template version without losing values.
	// A failure leaves the service on the file it already has (same policy as
	// the shell installer).
	if _, err := envconfig.Upgrade(configFile); err != nil {
		fmt.Fprintf(stderr, "install-service: warning: could not upgrade %s; leaving it unchanged: %v\n", configFile, err)
	}
	if opts.port != 8080 {
		if err := matchLocalSiteURL(configFile, opts.port); err != nil {
			return err
		}
	}
	if err := os.Chmod(configFile, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", configFile, err)
	}

	// 3. The systemd user unit.
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", unitDir, err)
	}
	unit := renderServiceUnit(opts.port)
	if err := os.WriteFile(unitFile, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitFile, err)
	}
	if err := opts.systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	if err := opts.systemctl("enable", "--now", installServiceUnit); err != nil {
		return fmt.Errorf("systemctl --user enable --now %s: %w", installServiceUnit, err)
	}
	// A re-run must switch a running old process to the freshly installed
	// binary; on a first install this is simply another clean start.
	if err := opts.systemctl("restart", installServiceUnit); err != nil {
		return fmt.Errorf("systemctl --user restart %s: %w", installServiceUnit, err)
	}

	// 4. What was installed.
	username := os.Getenv("USER")
	if username == "" {
		if current, err := user.Current(); err == nil {
			username = current.Username
		}
	}
	if username == "" {
		username = "your-user"
	}
	fmt.Fprintln(stdout, "install-service: installed and started")
	fmt.Fprintf(stdout, "  binary:  %s\n", targetBin)
	fmt.Fprintf(stdout, "  unit:    %s\n", unitFile)
	fmt.Fprintf(stdout, "  config:  %s\n", configFile)
	fmt.Fprintf(stdout, "  data:    %s\n", filepath.Join(dataDir, "data"))
	fmt.Fprintf(stdout, "  logs:    %s\n", filepath.Join(dataDir, "logs", "doory.log"))
	fmt.Fprintf(stdout, "  url:     http://localhost:%d\n", opts.port)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "  status:  systemctl --user status doory.service")
	fmt.Fprintf(stdout, "  logs:    tail -f %s\n", filepath.Join(dataDir, "logs", "doory.log"))
	fmt.Fprintf(stdout, "  stay up after logout:  loginctl enable-linger %s\n", username)
	return nil
}

func renderServiceUnit(port int) string {
	unit := `# Generated by 'doory install-service' — re-run it instead of editing.
[Unit]
Description=Doory (single binary: API, realtime co-editing, MCP, frontend)
Documentation=https://github.com/shaongitbd/doory
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# A user unit already runs as you; the binary and all state live under $HOME.
WorkingDirectory=%h/.doory
Environment=DOORY_DATA_DIR=%h/.doory/data
Environment=HEALTH_PORT={PORT}
# Configuration is read by the binary from ~/.config/doory/.env (D23); keep
# Environment= overrides above when a value must not be changed by that file.
# entrypoint: migrate, install the embedded content (templates, fonts, elements,
# audio) and then serve. Logs go to %h/.doory/logs/doory.log (rotated by the
# journal-less user unit: see logrotate note in the README).
ExecStart=%h/.local/bin/doory entrypoint :{PORT}
StandardOutput=append:%h/.doory/logs/doory.log
StandardError=append:%h/.doory/logs/doory.log
Restart=on-failure
RestartSec=2s
# Server-side PDF/image export spawns Chromium: it needs either its sandbox or
# EXPORT_CHROMIUM_NO_SANDBOX=true. No hardening directives that block that.

[Install]
WantedBy=default.target
`
	return strings.ReplaceAll(unit, "{PORT}", strconv.Itoa(port))
}

// installExecutable copies the running binary to target (atomic rename, so a
// running old unit does not hit ETXTBSY). Installing onto itself is a no-op.
func installExecutable(source, target string) error {
	if sourceInfo, err := os.Stat(source); err == nil {
		if targetInfo, terr := os.Stat(target); terr == nil && os.SameFile(sourceInfo, targetInfo) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open %s: %w", source, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(target), ".doory-install-*")
	if err != nil {
		return fmt.Errorf("create temporary binary in %s: %w", filepath.Dir(target), err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy %s: %w", source, err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary binary: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("install %s: %w", target, err)
	}
	return nil
}

func copyFileMode(source, target string, mode os.FileMode) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, data, mode)
}

// matchLocalSiteURL updates the template's localhost sample when the installer
// chose another port. A deliberately configured SITE_URL is left alone because
// only the exact sample value is replaced.
func matchLocalSiteURL(path string, port int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		if line == "SITE_URL=http://localhost:8080" {
			lines[i] = "SITE_URL=http://localhost:" + strconv.Itoa(port)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func runUserSystemctl(args ...string) error {
	command := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("%v: %s", err, message)
		}
		return err
	}
	return nil
}
