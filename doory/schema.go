// The OpenAPI document and Swagger UI (apps/core/urls.py + drf-spectacular).
//
// The document is generated from the pinned upstream at the version pin and
// embedded verbatim, so the served schema is the structural contract D8 talks
// about. The Swagger page is the generated drf-spectacular template, with the
// per-request CSRF token filled in.
package doory

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed openapi.yaml
var openapiYAML []byte

//go:embed openapi.json
var openapiJSON []byte

//go:embed swagger.html
var swaggerHTML string

func (a *App) openapiSchema(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "json" {
		w.Header().Set("Content-Type", "application/vnd.oai.openapi+json")
		w.Header().Set("Content-Disposition", `inline; filename="Doory API.json"`)
		w.Header().Set("Content-Length", itoa(len(openapiJSON)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openapiJSON)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.oai.openapi; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="Doory API.yaml"`)
	w.Header().Set("Content-Length", itoa(len(openapiYAML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiYAML)
}

func (a *App) apiDocs(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !a.Settings.Debug {
		if u == nil {
			writeDRF(w, http.StatusUnauthorized, "not_authenticated",
				"Authentication credentials were not provided.")
			return
		}
		if !u.IsStaff && !u.IsSuperuser {
			writeDRF(w, http.StatusForbidden, "permission_denied",
				"You do not have permission to perform this action.")
			return
		}
	}
	secret := ""
	if cookie, err := r.Cookie(csrfCookieName); err == nil && validCSRFSecret(cookie.Value) {
		secret = cookie.Value
	} else {
		secret = newCSRFSecret()
	}
	a.setCSRFCookie(w, secret)
	page := strings.ReplaceAll(swaggerHTML, "__CSRF_TOKEN__", secret)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", itoa(len(page)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(page))
}
