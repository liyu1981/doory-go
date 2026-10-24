// PostgreSQL smoke test (D4): the same binary serves the API against a
// Postgres DATABASE_URL. It boots the real App over httptest with the
// Postgres DSN, resets the schema, migrates and walks the routes that touch
// every dialect-sensitive construct: boolean columns (writes + comparisons),
// uuid columns (COALESCE / NULL), timestamptz scans and `?` placeholders.
//
// Skipped when the local parity container is not reachable, so `go test ./...`
// stays green without docker; `pnpm pg-smoke` runs it for the release gate.
package pgsmoke

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"doory-go/doory"
)

// pgURL is the local parity container (scripts/pg-oracle.sh); override with
// DOORY_PG_TEST_URL.
func pgURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("DOORY_PG_TEST_URL")
	if url == "" {
		url = "postgres://doory:doory@127.0.0.1:55432/doory_smoke"
	}
	probe, _, err := doory.OpenDB(url)
	if err != nil {
		t.Skipf("postgres not reachable (%v)", err)
	}
	if err := probe.Ping(); err != nil {
		probe.Close()
		t.Skipf("postgres not reachable (%v); start it with scripts/pg-oracle.sh", err)
	}
	probe.Close()
	return url
}

func resetSchema(t *testing.T, url string) {
	t.Helper()
	db, driver, err := doory.OpenDB(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if driver != "pgx" {
		t.Fatalf("expected the pgx driver, got %q", driver)
	}
	if _, err := db.Exec(`DROP SCHEMA public CASCADE`); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	if _, err := db.Exec(`CREATE SCHEMA public`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
}

func newServer(t *testing.T, url string) *httptest.Server {
	t.Helper()
	settings := doory.FromEnv()
	settings.DatabaseURL = url
	settings.MediaRoot = t.TempDir()
	settings.ExportStorageRoot = t.TempDir()
	settings.SecureCookies = false
	settings.SiteURL = "http://example.test"
	db, driver, err := doory.OpenDB(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := doory.Migrate(db, driver); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	app := doory.NewApp(settings, db)
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	return server
}

// client is one browser profile (cookies + CSRF echo).
type client struct {
	t    *testing.T
	base string
	http *http.Client
}

func newClient(t *testing.T, server *httptest.Server) *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: t, base: server.URL, http: &http.Client{Jar: jar}}
}

func (c *client) do(method, path, body string) (int, []byte) {
	c.t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	request, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token := c.csrf(); token != "" {
		request.Header.Set("X-CSRFToken", token)
		request.Header.Set("Referer", c.base+"/")
	}
	response, err := c.http.Do(request)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	payload := new(strings.Builder)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := response.Body.Read(buf)
		if n > 0 {
			payload.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	return response.StatusCode, []byte(payload.String())
}

func (c *client) csrf() string {
	u, _ := http.NewRequest("GET", c.base+"/", nil)
	for _, cookie := range c.http.Jar.Cookies(u.URL) {
		if cookie.Name == "csrftoken" {
			return cookie.Value
		}
	}
	return ""
}

func (c *client) expect(method, path, body string, want int) []byte {
	c.t.Helper()
	status, payload := c.do(method, path, body)
	if status != want {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, status, want, truncate(payload))
	}
	return payload
}

func truncate(payload []byte) string {
	if len(payload) > 240 {
		return string(payload[:240]) + "…"
	}
	return string(payload)
}

func TestPostgresServesTheAPI(t *testing.T) {
	url := pgURL(t)
	resetSchema(t, url)
	server := newServer(t, url)
	c := newClient(t, server)

	c.expect("GET", "/api/v1/auth/csrf/", "", http.StatusNoContent)
	c.expect("POST", "/api/v1/auth/register/",
		`{"email":"pg@example.com","password":"Tr0ub4dor&3xkcd9"}`, http.StatusCreated)
	c.expect("GET", "/api/v1/auth/me/", "", http.StatusOK)

	created := c.expect("POST", "/api/v1/designs/", `{"title":"Postgres Design"}`, http.StatusCreated)
	var design struct {
		ID       string `json:"id"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(created, &design); err != nil || design.ID == "" {
		t.Fatalf("design create: %v %s", err, truncate(created))
	}
	c.expect("GET", "/api/v1/designs/"+design.ID+"/", "", http.StatusOK)
	c.expect("GET", "/api/v1/designs/", "", http.StatusOK)

	// Share link with a password (boolean columns + uuid FK writes).
	link := c.expect("POST", "/api/v1/designs/"+design.ID+"/share-links/",
		`{"role":"view"}`, http.StatusCreated)
	var share struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(link, &share); err != nil || share.Token == "" {
		t.Fatalf("share link: %v %s", err, truncate(link))
	}
	c.expect("GET", "/api/v1/shared/"+share.Token+"/", "", http.StatusOK)

	// Comments (nullable parent uuid), folders (nullable parent uuid).
	c.expect("POST", "/api/v1/designs/"+design.ID+"/comments/",
		`{"body":"hello pin","page_id":"page-1","x":1,"y":2}`, http.StatusCreated)
	c.expect("GET", "/api/v1/designs/"+design.ID+"/comments/", "", http.StatusOK)
	c.expect("POST", "/api/v1/folders/", `{"name":"Postgres Folder"}`, http.StatusCreated)
	c.expect("GET", "/api/v1/folders/", "", http.StatusOK)

	// Versions (timestamp ordering, boolean is_auto).
	c.expect("POST", "/api/v1/designs/"+design.ID+"/versions/", `{"label":"v1"}`, http.StatusCreated)
	c.expect("GET", "/api/v1/designs/"+design.ID+"/versions/", "", http.StatusOK)

	// Public catalogs read through boolean filters (is_active/is_published).
	for _, path := range []string{
		"/api/v1/size-presets/", "/api/v1/elements/packs/", "/api/v1/templates/",
		"/api/v1/template-categories/", "/api/v1/fonts/", "/api/v1/assets/",
	} {
		c.expect("GET", path, "", http.StatusOK)
	}

	// Trash lifecycle: revision guard on update, trash/restore/purge.
	c.expect("PATCH", "/api/v1/designs/"+design.ID+"/",
		`{"document":{"schemaVersion":1,"width":10,"height":10,"pages":[{"id":"p","objects":[]}]},"base_revision":1}`,
		http.StatusOK)
	c.expect("DELETE", "/api/v1/designs/"+design.ID+"/", "", http.StatusNoContent)
	c.expect("POST", "/api/v1/designs/"+design.ID+"/restore/", "", http.StatusOK)
	c.expect("DELETE", "/api/v1/designs/"+design.ID+"/", "", http.StatusNoContent)
	c.expect("DELETE", "/api/v1/designs/"+design.ID+"/?permanent=1", "", http.StatusNoContent)
}

// TestPostgresUniqueAndConflictPaths covers the error paths that depend on
// driver error shapes (duplicate email, revision conflict).
func TestPostgresUniqueAndConflictPaths(t *testing.T) {
	url := pgURL(t)
	resetSchema(t, url)
	server := newServer(t, url)
	c := newClient(t, server)
	c.expect("GET", "/api/v1/auth/csrf/", "", http.StatusNoContent)
	c.expect("POST", "/api/v1/auth/register/",
		`{"email":"dup@example.com","password":"Tr0ub4dor&3xkcd9"}`, http.StatusCreated)
	c.expect("POST", "/api/v1/auth/register/",
		`{"email":"dup@example.com","password":"Tr0ub4dor&3xkcd9"}`, http.StatusBadRequest)

	created := c.expect("POST", "/api/v1/designs/", `{"title":"Conflicts"}`, http.StatusCreated)
	var design struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &design); err != nil {
		t.Fatalf("design: %v", err)
	}
	c.expect("PATCH", "/api/v1/designs/"+design.ID+"/",
		`{"document":{"schemaVersion":1,"width":10,"height":10,"pages":[{"id":"p","objects":[]}]},"base_revision":99}`,
		http.StatusConflict)
}

var _ = fmt.Sprintf
var _ = sql.ErrNoRows
