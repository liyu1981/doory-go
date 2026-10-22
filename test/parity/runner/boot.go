// Booting the two sides of a pair in isolation: the pinned oracle (Django,
// dev settings, SQLite workdir DB) and the freshly built Go binary. Both
// live on free ports, logs land in the scenario workdir, and the mirror
// stays pristine (PYTHONDONTWRITEBYTECODE=1, all writable paths inside the
// workdir).
package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Side is one running server of the pair. Each named jar models one
// browser profile (separate sessions).
type Side struct {
	Name string
	Base string
	Jars map[string]http.CookieJar
	cmd  *exec.Cmd
	log  *os.File
}

// jar returns (creating on demand) the named cookie jar.
func (s *Side) jar(name string) http.CookieJar {
	if name == "" {
		name = "main"
	}
	if s.Jars == nil {
		s.Jars = map[string]http.CookieJar{}
	}
	if j, ok := s.Jars[name]; ok {
		return j
	}
	j := newJar()
	s.Jars[name] = j
	return j
}

// bootPair builds the Go binary, migrates its workdir DB, starts both
// servers and waits until each answers /api/v1/health/.
func (r *Run) bootPair(sc *Scenario, renderURL string) (*Side, *Side, error) {
	binPath := filepath.Join(r.WorkDir, "doory-bin")
	build := exec.Command("go", "build", "-o", binPath, "./cmd/doory")
	build.Dir = r.Repo
	if out, err := build.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("build go binary: %v\n%s", err, out)
	}

	goDB := "sqlite:///" + filepath.Join(r.WorkDir, "go.sqlite3")
	migrate := exec.Command(binPath, "migrate")
	migrate.Dir = r.Repo
	migrate.Env = append(os.Environ(), "DATABASE_URL="+goDB)
	if out, err := migrate.CombinedOutput(); err != nil {
		return nil, nil, fmt.Errorf("go migrate: %v\n%s", err, out)
	}

	oraclePort, err := freePort()
	if err != nil {
		return nil, nil, err
	}
	goPort, err := freePort()
	if err != nil {
		return nil, nil, err
	}

	media := filepath.Join(r.WorkDir, "media")
	exports := filepath.Join(r.WorkDir, "exports")
	for _, d := range []string{media, exports} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, nil, err
		}
	}

	oracle := &Side{Name: "oracle", Base: fmt.Sprintf("http://127.0.0.1:%d", oraclePort)}
	oracleLog := filepath.Join(r.WorkDir, "oracle.log")
	oracle.log, _ = os.Create(oracleLog)
	oracleCmd := exec.Command("bash", "tools/oracle/serve.sh")
	oracleCmd.Dir = r.Repo
	oracleCmd.Env = append(os.Environ(),
		fmt.Sprintf("ORACLE_PORT=%d", oraclePort),
		"ORACLE_SETTINGS=doory.settings.prod",
		"ORACLE_DATABASE_URL=sqlite:///"+filepath.Join(r.WorkDir, "oracle.sqlite3"),
		"MEDIA_ROOT="+media,
		"EXPORT_STORAGE_ROOT="+exports,
	)
	oracleCmd.Env = append(oracleCmd.Env,
		// The oracle runs the DEPLOYED settings family (prod): dev hardcodes
		// DEBUG pages and console-only behavior that would not be honest
		// parity targets. Plain-HTTP local testing needs the two documented
		// prod opt-outs (prod.py: "may only be switched off to try Doory
		// over plain HTTP locally").
		"DJANGO_SECRET_KEY=parity-oracle-secret-key-0123456789",
		"SECURE_SSL_REDIRECT=false",
		"SECURE_COOKIES=false",
	)
	oracleCmd.Env = append(oracleCmd.Env, scenarioEnv(sc)...)
	if renderURL != "" {
		oracleCmd.Env = append(oracleCmd.Env,
			"EXPORT_RENDER_PAGE_URL="+renderURL,
			// The render page's origin must be allowed by the export policy
			// (upstream: the SITE origin serving render.html; here: the
			// runner's stub server).
			"EXPORT_ALLOWED_ORIGINS="+strings.Join(append([]string{originOf(renderURL)}, scenarioAllowed(sc)...), ","))
	}
	oracleCmd.Stdout = oracle.log
	oracleCmd.Stderr = oracle.log
	oracleCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := oracleCmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start oracle: %w", err)
	}
	oracle.cmd = oracleCmd

	goSide := &Side{Name: "go", Base: fmt.Sprintf("http://127.0.0.1:%d", goPort)}
	goLog := filepath.Join(r.WorkDir, "go.log")
	goSide.log, _ = os.Create(goLog)
	serve := exec.Command(binPath, "serve", fmt.Sprintf("127.0.0.1:%d", goPort))
	serve.Dir = r.Repo
	serve.Env = append(os.Environ(), "DATABASE_URL="+goDB,
		fmt.Sprintf("DOORY_ADDR=127.0.0.1:%d", goPort),
		// Same plain-HTTP local-testing opt-out as the oracle (prod.py docs).
		"SECURE_COOKIES=false")
	serve.Env = append(serve.Env, scenarioEnv(sc)...)
	if renderURL != "" {
		serve.Env = append(serve.Env,
			"EXPORT_RENDER_PAGE_URL="+renderURL,
			"EXPORT_ALLOWED_ORIGINS="+strings.Join(append([]string{originOf(renderURL)}, scenarioAllowed(sc)...), ","))
	}
	serve.Stdout = goSide.log
	serve.Stderr = goSide.log
	serve.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := serve.Start(); err != nil {
		r.shutdown(oracle)
		return nil, nil, fmt.Errorf("start go server: %w", err)
	}
	goSide.cmd = serve

	if err := waitHealthy(oracle); err != nil {
		r.shutdown(oracle, goSide)
		return nil, nil, err
	}
	if err := waitHealthy(goSide); err != nil {
		r.shutdown(oracle, goSide)
		return nil, nil, err
	}
	return oracle, goSide, nil
}

// originOf returns scheme://host[:port] of a URL.
func originOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return raw[:i+3] + rest
	}
	return raw
}

// scenarioAllowed preserves any scenario-declared extra origins.
func scenarioAllowed(sc *Scenario) []string {
	if v, ok := sc.Env["EXPORT_ALLOWED_ORIGINS"]; ok && v != "" {
		return strings.Split(v, ",")
	}
	return nil
}

func scenarioEnv(sc *Scenario) []string {
	var out []string
	for k, v := range sc.Env {
		out = append(out, k+"="+v)
	}
	return out
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy polls the side's health endpoint until 200 (boot can take a
// while: Django cold import + daphne).
func waitHealthy(s *Side) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(s.Base + "/api/v1/health/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("%s never became healthy: %v (log: %s)", s.Name, lastErr, s.log.Name())
}

// shutdown kills both sides' process groups.
func (r *Run) shutdown(sides ...*Side) {
	for _, s := range sides {
		if s.cmd != nil && s.cmd.Process != nil {
			pgid := -s.cmd.Process.Pid
			_ = syscall.Kill(pgid, syscall.SIGTERM)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			go func() {
				_, _ = s.cmd.Process.Wait()
				cancel()
			}()
			<-ctx.Done()
			_ = syscall.Kill(pgid, syscall.SIGKILL)
		}
		if s.log != nil {
			s.log.Close()
		}
	}
}
