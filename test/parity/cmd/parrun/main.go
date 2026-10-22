// Command parrun runs paired oracle/Go scenarios (the parity-fast /
// parity targets).
//
//	parrun <scenario.toml|dir>...   run scenarios (serially for now)
//	parrun -fast <paths>...         only scenarios tagged "fast"
//
// Requires: .oracle/venv (pnpm oracle:setup) for the oracle side; the Go
// side is built per run. Each scenario gets its own workdir DBs/media under
// .oracle/parity/; the mirror stays pristine.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"doory-go/internal/parity"
	"doory-go/test/parity/runner"
)

func main() {
	fastOnly := false
	var paths []string
	for _, a := range os.Args[1:] {
		if a == "-fast" {
			fastOnly = true
			continue
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{"test/parity/scenarios"}
	}

	repo, err := parity.RepoRoot()
	if err != nil {
		fatal(err)
	}
	if _, err := os.Stat(repo + "/.oracle/venv/bin/python"); err != nil {
		fatal(fmt.Errorf("oracle venv missing — run 'pnpm oracle:setup'"))
	}

	// Serve the deterministic render-page stub (image export protocol).
	renderDir := filepath.Join(repo, "test", "parity", "testdata", "render")
	renderPort, err := freeTCPPort()
	if err != nil {
		fatal(err)
	}
	renderSrv := &http.Server{Addr: "127.0.0.1:" + renderPort,
		Handler: http.FileServer(http.Dir(renderDir))}
	go func() { _ = renderSrv.ListenAndServe() }()
	defer renderSrv.Close()
	renderURL := "http://127.0.0.1:" + renderPort + "/render.html"

	files, err := runner.Discover(paths, fastOnly)
	if err != nil {
		fatal(err)
	}
	if len(files) == 0 {
		fmt.Println("parrun: no scenarios")
		return
	}

	failed := 0
	for _, file := range files {
		sc, err := runner.Load(file)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("scenario %s (%s, driver=%s)\n", sc.Name, file, sc.Driver)
		problems, err := runner.Execute(repo, sc, renderURL)
		if err != nil {
			fmt.Printf("  ERROR: %v\n", err)
			failed++
			continue
		}
		if len(problems) > 0 {
			failed++
			for _, p := range problems {
				fmt.Printf("  FAIL %s\n", p)
			}
		} else {
			fmt.Printf("  PASS %s (%d steps)\n", sc.Name, len(sc.Steps))
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "parrun: %d/%d scenario(s) failed\n", failed, len(files))
		os.Exit(1)
	}
	fmt.Printf("parrun: %d scenario(s) green\n", len(files))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "parrun:", err)
	os.Exit(1)
}

func freeTCPPort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return fmt.Sprintf("%d", l.Addr().(*net.TCPAddr).Port), nil
}
