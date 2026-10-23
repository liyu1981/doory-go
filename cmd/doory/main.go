// Command doory is the single-binary release (D1): the API server, the
// database migrator and the future CLI subcommands in one artifact.
//
// Subcommands:
//
//	serve     HTTP + (later) WS + MCP on one listener (D1 flags split them)
//	migrate   apply the embedded schema for the DATABASE_URL dialect (D4)
//	version   print the port version and its upstream pin
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"

	"doory-go/doory"
	"doory-go/internal/dooryversion"
	"doory-go/internal/renderworker"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	args := os.Args[2:]

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
	case "migrate":
		if err := doory.MigrateFromEnv(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate:", err)
			os.Exit(1)
		}
		fmt.Println("migrate: ok")
	case "serve":
		addr := envOr("DOORY_ADDR", ":8000")
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
		fmt.Printf("doory: serving on %s (db: %s)\n", addr, s.DatabaseURL)
		if err := http.ListenAndServe(addr, app.Handler()); err != nil {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want serve|migrate|version)\n", cmd)
		os.Exit(2)
	}
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
