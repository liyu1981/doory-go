package doory

import (
	"net/http/httptest"
	"testing"
)

// The Allow rule (S4 semantics table, source-verified against DRF/Django):
// api_view lists methods + OPTIONS; APIView/ViewSet aliases head=get and
// always allows OPTIONS. Evidence: probe 0001 (health "GET, OPTIONS",
// csrf "GET, HEAD, OPTIONS", designs list "GET, POST, HEAD, OPTIONS").
func TestAllowHeader(t *testing.T) {
	cases := []struct {
		name  string
		route Route
		want  string
	}{
		{"api_view GET", Route{Methods: []string{"GET"}, Style: AllowAPIView}, "GET, OPTIONS"},
		{"api_view POST+GET", Route{Methods: []string{"GET", "POST"}, Style: AllowAPIView}, "GET, POST, OPTIONS"},
		{"class get only", Route{Methods: []string{"GET"}, Style: AllowClass}, "GET, HEAD, OPTIONS"},
		{"class get+post (ViewSet list)", Route{Methods: []string{"GET", "POST"}, Style: AllowClass}, "GET, POST, HEAD, OPTIONS"},
		{"class put/patch/delete (ViewSet detail)", Route{Methods: []string{"GET", "PUT", "PATCH", "DELETE"}, Style: AllowClass}, "GET, PUT, PATCH, DELETE, HEAD, OPTIONS"},
		{"class post only (no head alias)", Route{Methods: []string{"POST"}, Style: AllowClass}, "POST, OPTIONS"},
	}
	for _, c := range cases {
		if got := c.route.AllowHeader(); got != c.want {
			t.Errorf("%s: Allow = %q, want %q", c.name, got, c.want)
		}
	}
}

// Two exact JSON dialects (probe-proven): DRF compact envelope vs Django
// JsonResponse's default separators (", " / ": ").
func TestEnvelopeBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeDRF(rec, 405, "method_not_allowed", `Method "POST" not allowed.`)
	want := `{"error":{"code":"method_not_allowed","message":"Method \"POST\" not allowed."}}`
	if rec.Body.String() != want {
		t.Errorf("DRF envelope:\n got %s\nwant %s", rec.Body.String(), want)
	}

	rec = httptest.NewRecorder()
	writeDjangoJSON(rec, 404, "not_found", "Not found.")
	want = `{"error": {"code": "not_found", "message": "Not found."}}`
	if rec.Body.String() != want {
		t.Errorf("Django JsonResponse envelope:\n got %s\nwant %s", rec.Body.String(), want)
	}
}

// patch_vary_headers semantics: append, dedupe, lowercase 'origin' from cors
// after 'Cookie' from session/csrf (probe 0001: "Vary: Cookie, origin").
func TestPatchVary(t *testing.T) {
	rec := httptest.NewRecorder()
	patchVary(rec, "Cookie")
	patchVary(rec, "origin")
	if got := rec.Header().Get("Vary"); got != "Cookie, origin" {
		t.Errorf("Vary = %q, want %q", got, "Cookie, origin")
	}
	patchVary(rec, "origin") // no duplicate
	if got := rec.Header().Get("Vary"); got != "Cookie, origin" {
		t.Errorf("Vary after dedupe = %q", got)
	}
}

// CSRF secrets: 32 chars from [A-Za-z0-9] (django/middleware/csrf.py
// CSRF_SECRET_LENGTH=32, CSRF_ALLOWED_CHARS=ascii_letters+digits; probe 0001
// step 03 cookie value).
func TestCSRFSecret(t *testing.T) {
	secret := newCSRFSecret()
	if len(secret) != 32 {
		t.Fatalf("secret length %d, want 32", len(secret))
	}
	if !validCSRFSecret(secret) {
		t.Errorf("generated secret rejected: %q", secret)
	}
	for _, bad := range []string{"", "short", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!", "abc-def-ghi-jkl-mno-pqr-stu-vw"} {
		if validCSRFSecret(bad) {
			t.Errorf("accepted invalid secret %q", bad)
		}
	}
}

// env coercions mirror django-environ (settings contract).
func TestEnvCoercions(t *testing.T) {
	t.Setenv("DOORY_TEST_BOOL_T", "yes")
	t.Setenv("DOORY_TEST_BOOL_F", "off")
	t.Setenv("DOORY_TEST_LIST", " a ,b,,")
	if !envBool("DOORY_TEST_BOOL_T", false) {
		t.Error("yes should be true")
	}
	if envBool("DOORY_TEST_BOOL_F", true) {
		t.Error("off should be false")
	}
	got := envList("DOORY_TEST_LIST", nil)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("envList = %v, want [a b]", got)
	}
}

// EmailDelivers: base.py hardcodes console (never delivers) — the deployed
// default makes features.email_verification false (probed on prod settings).
func TestEmailDelivers(t *testing.T) {
	s := &Settings{EmailBackend: "django.core.mail.backends.console.EmailBackend"}
	if s.EmailDelivers() {
		t.Error("console backend must not deliver")
	}
	s.EmailBackend = "django.core.mail.backends.smtp.EmailBackend"
	if !s.EmailDelivers() {
		t.Error("smtp backend delivers")
	}
}
