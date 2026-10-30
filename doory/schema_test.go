package doory

import (
	"net/http"
	"strings"
	"testing"
)

func TestOpenAPISchemaAndSwaggerPage(t *testing.T) {
	env := newAdminEnv(t)

	res, body := env.get("/api/v1/schema/")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "openapi") ||
		!strings.Contains(body, "/api/v1/auth/google/") || !strings.Contains(body, "openapi:") {
		t.Fatalf("schema yaml = %d %q %.120s", res.StatusCode, res.Header.Get("Content-Type"), body)
	}
	res, body = env.get("/api/v1/schema/?format=json")
	if res.StatusCode != http.StatusOK || !strings.Contains(res.Header.Get("Content-Type"), "application/vnd.oai.openapi+json") ||
		!strings.Contains(body, `"/api/v1/auth/google/"`) {
		t.Fatalf("schema json = %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}

	res, _ = env.get("/api/v1/docs/")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous docs = %d, want 401", res.StatusCode)
	}
	ordinary := env.makeUser("docs@example.com", false, false)
	env.loginAs(ordinary)
	res, _ = env.get("/api/v1/docs/")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("ordinary docs = %d, want 403", res.StatusCode)
	}
	staff := env.makeUser("staff-docs@example.com", true, false)
	env.loginAs(staff)
	res, body = env.get("/api/v1/docs/")
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "swagger-ui-dist@5.17.14") ||
		strings.Contains(body, "@latest") || strings.Contains(body, "__CSRF_TOKEN__") {
		t.Fatalf("staff docs = %d %.200s", res.StatusCode, body)
	}

	env.settings.Debug = true
	res, _ = env.get("/api/v1/docs/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("debug docs = %d, want 200", res.StatusCode)
	}
}
