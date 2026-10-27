// Command doory is the single-binary release (D1): the API server, the
// database migrator and the future CLI subcommands in one artifact.
//
// Subcommands:
//
//	serve                 HTTP + WS + MCP on one listener (D1 flags split them)
//	entrypoint            container prep (wait for the DB, migrate, optional catalog
//	                      bootstraps driven by DOORY_SYNC_*/DOORY_SEED_*) then serve
//	healthcheck           container probe (HEALTH_PORT/HEALTH_PATH, exit 0 on 200)
//	migrate               apply the embedded schema for the DATABASE_URL dialect (D4)
//	config                generate/upgrade the optional .env file (init, upgrade)
//	version               print the port version and its upstream pin
//	help                  list the management commands
//
// Management commands (M13) keep their upstream names (purge_trash,
// seed_size_presets, sync_models, …) and option spelling; the cli-driver
// compares their output, database rows and media files against the oracle.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"doory-go/doory"
	"doory-go/internal/dooryversion"
	"doory-go/internal/envconfig"
	"doory-go/internal/renderworker"
)

func main() {
	argv, global, err := takeGlobalFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "doory:", err)
		os.Exit(2)
	}
	cmd, args := splitArgs(argv)
	if cmd == "" || cmd == "help" || cmd == "--help" || cmd == "-h" {
		// A bare binary prints the command overview (like manage.py) instead of
		// guessing what to run.
		doory.PrintCLIHelp(os.Stdout)
		return
	}
	if err := loadEnvForCommand(cmd, global); err != nil {
		fmt.Fprintln(os.Stderr, "doory:", err)
		os.Exit(1)
	}
	switch cmd {
	case "render-worker":
		// Hidden: the per-render subprocess (renderer.py's worker model).
		// Usage: doory render-worker <spec.json>; writes the report and the
		// output file, exits 0/1 on ok/error — never talks to the API.
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "render-worker: missing spec path")
			os.Exit(2)
		}
		report, err := renderworker.Run(args[0])
		if report != nil {
			if raw, jerr := json.Marshal(report); jerr == nil {
				_ = os.WriteFile(specReportPath(args[0]), raw, 0o644)
			}
		}
		if err != nil || report == nil || report.Status != "ok" {
			os.Exit(1)
		}
		os.Exit(0)
	case "version":
		fmt.Printf("doory-go %s (upstream %s)\n", dooryversion.Version, dooryversion.UpstreamCommit)
	case "config":
		os.Exit(doory.RunConfigCommand(args, os.Stdout, os.Stderr))
	case "migrate":
		if err := doory.MigrateFromEnv(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("migrate: ok")
	case "healthcheck":
		// backend/healthcheck.py
		os.Exit(doory.Healthcheck())
	case "entrypoint":
		// --prepare: prepare and exit 0 (probe the bootstraps without serving);
		// --prepare-only is accepted as an alias. Extra args are ignored.
		prepareOnly := false
		addr := envOr("DOORY_ADDR", ":8111")
		for _, arg := range args {
			if arg == "--prepare" || arg == "--prepare-only" {
				prepareOnly = true
				continue
			}
			// A positional address (deploy/doory.service runs
			// `doory entrypoint :8080`), like `serve`.
			if len(arg) > 0 && arg[0] != '-' && addr == envOr("DOORY_ADDR", ":8111") {
				addr = arg
			}
		}
		s := doory.FromEnv()
		db, _, err := doory.OpenDB(s.DatabaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "entrypoint:", err)
			os.Exit(1)
		}
		defer db.Close()
		if err := doory.WaitForDatabase(db, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		app := doory.NewApp(s, db)
		if err := doory.PrepareApp(app, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "entrypoint:", err)
			os.Exit(1)
		}
		if prepareOnly {
			os.Exit(0)
		}
		fmt.Printf("doory: serving on %s (db: %s)\n", addr, s.DatabaseURL)
		if err := http.ListenAndServe(addr, app.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	case "serve":
		addr := envOr("DOORY_ADDR", ":8111")
		for _, a := range args {
			if len(a) > 0 && a[0] != '-' {
				addr = a
				break
			}
		}
		s := doory.FromEnv()
		db, _, err := doory.OpenDB(s.DatabaseURL)
		if err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
		defer db.Close()
		app := doory.NewApp(s, db)
		// `serve` deliberately does not install content (parity runs against
		// an unseeded oracle too); point at the one command that does.
		var templates int
		if err := db.QueryRow(`SELECT COUNT(*) FROM gallery_template`).Scan(&templates); err == nil && templates == 0 {
			fmt.Println("doory: no starter templates yet — run 'doory seed_templates' (or use 'doory entrypoint', which installs the embedded content on start)")
		}
		fmt.Printf("doory: serving on %s (db: %s)\n", addr, s.DatabaseURL)
		if err := http.ListenAndServe(addr, app.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	default:
		// Everything else is a management command (Django's manage.py <cmd>).
		os.Exit(doory.RunCLIFromEnv(argv))
	}
}

// globalOptions are the flags accepted before (or after) any subcommand.
type globalOptions struct {
	envFile   string
	noEnvFile bool
}

// takeGlobalFlags removes the global --env-file/--no-env-file options from
// argv. They may appear anywhere before a `--` terminator; the remaining args
// keep their order.
func takeGlobalFlags(argv []string) ([]string, globalOptions, error) {
	var opts globalOptions
	envFileSeen := false
	rest := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			rest = append(rest, argv[i:]...)
			break
		}
		switch {
		case arg == "--env-file":
			if i+1 >= len(argv) {
				return nil, opts, fmt.Errorf("--env-file needs a path")
			}
			envFileSeen = true
			opts.envFile = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--env-file="):
			envFileSeen = true
			opts.envFile = strings.TrimPrefix(arg, "--env-file=")
		case arg == "--no-env-file":
			opts.noEnvFile = true
		default:
			rest = append(rest, arg)
		}
	}
	if envFileSeen && strings.TrimSpace(opts.envFile) == "" {
		return nil, opts, fmt.Errorf("--env-file needs a path")
	}
	if opts.noEnvFile && envFileSeen {
		return nil, opts, fmt.Errorf("--env-file and --no-env-file are mutually exclusive")
	}
	return rest, opts, nil
}

// loadEnvForCommand fills missing process-environment variables from the
// selected dotenv file. Commands that do not read configuration skip it, so
// `doory config` and `doory version` work even with a broken user env file.
func loadEnvForCommand(cmd string, opts globalOptions) error {
	switch cmd {
	case "version", "config":
		return nil
	}
	if opts.noEnvFile {
		return nil
	}
	_, err := envconfig.Load(opts.envFile)
	return err
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// specReportPath reads the report path out of a spec file (the worker
// writes where the spec says).
func specReportPath(specPath string) string {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return ""
	}
	var spec struct {
		ReportPath string `json:"report_path"`
	}
	if json.Unmarshal(raw, &spec) != nil {
		return ""
	}
	return spec.ReportPath
}

// splitArgs splits the command line after the program name into the
// subcommand and its arguments; "" means no subcommand was given (main then
// prints the usage text). Kept separate from main so the no-argument case is
// testable: slicing os.Args[2:] on a bare invocation panicked.
func splitArgs(argv []string) (string, []string) {
	if len(argv) == 0 {
		return "", nil
	}
	return argv[0], argv[1:]
}
