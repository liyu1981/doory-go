// Integration harness for the admin site plus the behaviours ported from the
// upstream apps/*/tests/test_admin.py files (G31). Every request goes through
// the real stack (router, middleware, session, CSRF) against a migrated
// SQLite database.
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const adminTestPassword = "correct-horse-battery-1"

type adminEnv struct {
	t        *testing.T
	app      *App
	settings *Settings
	db       *sql.DB
	server   *httptest.Server
	client   *http.Client
	csrf     string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATABASE_URL", "sqlite://"+filepath.Join(dir, "db.sqlite3"))
	t.Setenv("MEDIA_ROOT", filepath.Join(dir, "media"))
	t.Setenv("EXPORT_STORAGE_ROOT", filepath.Join(dir, "exports"))
	t.Setenv("DOORY_DATA_DIR", dir)
	t.Setenv("DJANGO_SECRET_KEY", "admin-test-secret-key-0123456789")
	t.Setenv("SECURE_COOKIES", "false")
	t.Setenv("AI_SECRETS_KEY", "")
	t.Setenv("MCP_ENABLED", "false")
	settings := FromEnv()
	db, driver, err := OpenDB(settings.DatabaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := Migrate(db, driver); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	app := NewApp(settings, db)
	server := httptest.NewServer(app.Handler())
	jar, _ := cookiejar.New(nil)
	e := &adminEnv{
		t: t, app: app, settings: settings, db: db, server: server,
		client: &http.Client{
			Jar: jar,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	t.Cleanup(func() {
		server.Close()
		_ = db.Close()
	})
	// Every admin request mints the CSRF cookie when absent; seed a valid one.
	e.csrf = newCSRFSecret()
	e.setCookie(&http.Cookie{Name: csrfCookieName, Value: e.csrf, Path: "/"})
	return e
}

func (e *adminEnv) setCookie(cookie *http.Cookie) {
	u, _ := url.Parse(e.server.URL + "/")
	e.client.Jar.SetCookies(u, []*http.Cookie{cookie})
}

func (e *adminEnv) url(path string) string { return e.server.URL + path }

func (e *adminEnv) do(method, path string, body io.Reader, contentType string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, e.url(path), body)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	data, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	return res, string(data)
}

func (e *adminEnv) get(path string) (*http.Response, string) {
	return e.do(http.MethodGet, path, nil, "")
}

func (e *adminEnv) postForm(path string, values url.Values) (*http.Response, string) {
	values.Set("csrfmiddlewaretoken", e.csrf)
	return e.do(http.MethodPost, path, strings.NewReader(values.Encode()),
		"application/x-www-form-urlencoded")
}

func (e *adminEnv) postMultipart(path string, fields url.Values,
	files map[string][2]string,
) (*http.Response, string) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fields.Set("csrfmiddlewaretoken", e.csrf)
	for key, values := range fields {
		for _, value := range values {
			_ = writer.WriteField(key, value)
		}
	}
	for field, file := range files {
		part, err := writer.CreateFormFile(field, file[0])
		if err != nil {
			e.t.Fatalf("multipart %s: %v", field, err)
		}
		_, _ = part.Write([]byte(file[1]))
	}
	_ = writer.Close()
	return e.do(http.MethodPost, path, &buf, writer.FormDataContentType())
}

// adminUpload is one multipart file part (several may share a field name,
// which is how the bulk SVG upload sends its `files` list).
type adminFilePart struct {
	field    string
	filename string
	body     string
}

func (e *adminEnv) postFiles(path string, fields url.Values,
	files []adminFilePart,
) (*http.Response, string) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	fields.Set("csrfmiddlewaretoken", e.csrf)
	for key, values := range fields {
		for _, value := range values {
			_ = writer.WriteField(key, value)
		}
	}
	for _, file := range files {
		part, err := writer.CreateFormFile(file.field, file.filename)
		if err != nil {
			e.t.Fatalf("multipart %s: %v", file.field, err)
		}
		_, _ = part.Write([]byte(file.body))
	}
	_ = writer.Close()
	return e.do(http.MethodPost, path, &buf, writer.FormDataContentType())
}

// makeUser inserts an account (create_user semantics) with the given flags.
func (e *adminEnv) makeUser(email string, staff, super bool) *UserRow {
	e.t.Helper()
	u, err := e.app.createUser(email, adminTestPassword, "")
	if err != nil {
		e.t.Fatalf("create user %s: %v", email, err)
	}
	if staff || super {
		if _, err := e.db.Exec(`UPDATE accounts_user SET is_staff = ?, is_superuser = ?,
			is_email_verified = ? WHERE id = ?`, boolSQL(staff || super), boolSQL(super), boolSQL(super), u.ID); err != nil {
			e.t.Fatalf("flags %s: %v", email, err)
		}
	}
	u.IsStaff, u.IsSuperuser = staff || super, super
	return u
}

// loginAs creates a session row directly (Django's Client.force_login) and
// installs the cookie.
func (e *adminEnv) loginAs(u *UserRow) {
	e.t.Helper()
	key := newSessionKey()
	if err := putSession(e.db, key, u.ID, sessionAuthHash(u.Password)); err != nil {
		e.t.Fatalf("session: %v", err)
	}
	e.setCookie(&http.Cookie{Name: sessionCookieName, Value: key, Path: "/"})
}

func (e *adminEnv) adminUser() *UserRow {
	u := e.makeUser("root@example.com", true, true)
	e.loginAs(u)
	return u
}

// grant gives the user the named permissions (app.codename).
func (e *adminEnv) grant(u *UserRow, perms ...string) {
	e.t.Helper()
	for _, perm := range perms {
		res, err := e.db.Exec(`INSERT INTO accounts_user_user_permissions (user_id, permission_id)
			SELECT ?, p.id FROM auth_permission p
			JOIN django_content_type ct ON ct.id = p.content_type_id
			WHERE ct.app_label || '.' || p.codename = ?`, u.ID, perm)
		if err != nil {
			e.t.Fatalf("grant %s: %v", perm, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			e.t.Fatalf("permission %s does not exist", perm)
		}
	}
}

func (e *adminEnv) mustGet(path string) string {
	e.t.Helper()
	res, body := e.get(path)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("GET %s = %d, want 200; body: %.500s", path, res.StatusCode, body)
	}
	return body
}

func (e *adminEnv) wantStatus(res *http.Response, want int, label string, body string) {
	e.t.Helper()
	if res.StatusCode != want {
		e.t.Fatalf("%s = %d, want %d; body: %.800s", label, res.StatusCode, want, body)
	}
}

func (e *adminEnv) wantRedirect(res *http.Response, location, label string) {
	e.t.Helper()
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("%s = %d, want 302", label, res.StatusCode)
	}
	if location != "" && res.Header.Get("Location") != location {
		e.t.Fatalf("%s Location = %q, want %q", label, res.Header.Get("Location"), location)
	}
}

// adminModelPaths is the 19 registered admin models + auth.Group.
var adminTestModels = []string{
	"accounts/user",
	"auth/group",
	"designs/design", "designs/designversion", "designs/folder",
	"assets/asset", "brand/brandkit", "sharing/sharelink", "sharing/comment",
	"gallery/templatecategory", "gallery/template",
	"elements/elementpack", "elements/element",
	"fonts/fontfamily",
	"presets/sizepresetgroup", "presets/sizepreset",
	"apitokens/apitoken",
	"ai/provider", "ai/userprovider", "ai/aicall",
}

func TestAdminChangelistsAndAddPagesRender(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	for _, path := range adminTestModels {
		t.Run(strings.ReplaceAll(path, "/", "_")+"_changelist", func(t *testing.T) {
			e.mustGet("/admin/" + path + "/")
		})
	}
	// has_add_permission False models answer 403, like Django's add_view.
	noAdd := map[string]bool{
		"designs/design": true, "designs/designversion": true, "designs/folder": true,
		"assets/asset":   true,
		"brand/brandkit": true, "sharing/sharelink": true, "sharing/comment": true,
		"apitokens/apitoken": true, "ai/userprovider": true, "ai/aicall": true,
	}
	for _, path := range adminTestModels {
		t.Run(strings.ReplaceAll(path, "/", "_")+"_add", func(t *testing.T) {
			res, body := e.get("/admin/" + path + "/add/")
			if noAdd[path] {
				e.wantStatus(res, http.StatusForbidden, "add", body)
				return
			}
			e.wantStatus(res, http.StatusOK, "add", body)
		})
	}
}

func TestAdminIndexAndSearchRender(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	index := e.mustGet("/admin/")
	if !strings.Contains(index, "Accounts") || !strings.Contains(index, "AI") {
		t.Fatalf("index does not list the apps: %.500s", index)
	}
	e.mustGet("/admin/accounts/")
	e.mustGet("/admin/search/?q=root")
}

func TestAdminRequiresStaff(t *testing.T) {
	e := newAdminEnv(t)
	user := e.makeUser("plain@example.com", false, false)
	e.loginAs(user)
	res, body := e.get("/admin/accounts/user/")
	e.wantStatus(res, http.StatusFound, "changelist as non-staff", body)
	if !strings.Contains(res.Header.Get("Location"), "/admin/login/") {
		t.Fatalf("Location = %q, want the login page", res.Header.Get("Location"))
	}
}

func TestAdminLoginAcceptsMixedCaseEmail(t *testing.T) {
	e := newAdminEnv(t)
	e.makeUser("root@example.com", true, true)
	res, body := e.postForm("/admin/login/?next=/admin/", url.Values{
		"username": {"ROOT@Example.com"}, "password": {adminTestPassword}, "next": {"/admin/"},
	})
	e.wantStatus(res, http.StatusFound, "login", body)
	if got := res.Header.Get("Location"); got != "/admin/" {
		t.Fatalf("Location = %q, want /admin/", got)
	}
}

func TestAdminMissingPermissionIs403(t *testing.T) {
	e := newAdminEnv(t)
	u := e.makeUser("viewer@example.com", true, false)
	e.loginAs(u)
	res, body := e.get("/admin/accounts/user/")
	e.wantStatus(res, http.StatusForbidden, "changelist without perms", body)
	e.grant(u, "accounts.view_user")
	e.mustGet("/admin/accounts/user/")
	res, body = e.get("/admin/accounts/user/add/")
	e.wantStatus(res, http.StatusForbidden, "add without add perm", body)
}

// jsonBody is used by the AI provider form (style_config etc.).
func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return string(data)
}

var _ = fmt.Sprintf
var _ = time.Now
