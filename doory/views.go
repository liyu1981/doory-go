// Kernel views.
//
// Ports backend/apps/core/views.py (health, config)
// Ports backend/apps/accounts/views.py (CsrfView)
package doory

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"time"
)

// mediaURL mirrors settings.MEDIA_URL ("/media/", base.py).
const mediaURL = "/media/"

// health ports @api_view(["GET"]) health: SELECT 1 liveness+db check,
// {"status": "ok"}. Probe 0001 step 01.
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.DB.Ping(); err != nil {
		// Oracle behavior on db failure is a 500 envelope
		// (handler500 -> server_error); probe with a broken DB is pending.
		writeDRF(w, http.StatusInternalServerError, "server_error",
			"Something went wrong on our side.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	body := encodeCompactJSON(map[string]string{"status": "ok"})
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

// configResponse mirrors the dict literal in apps/core/views.py config()
// (key order = JSON order: google_client_id, features, exports, limits,
// assets, ai).
type configResponse struct {
	GoogleClientID string         `json:"google_client_id"`
	Features       configFeatures `json:"features"`
	Exports        configExports  `json:"exports"`
	Limits         configLimits   `json:"limits"`
	Assets         configAssets   `json:"assets"`
	AI             aiConfig       `json:"ai"`
}

type configFeatures struct {
	StockPhotos       bool `json:"stock_photos"`
	ServerPDF         bool `json:"server_pdf"`
	GoogleLogin       bool `json:"google_login"`
	EmailVerification bool `json:"email_verification"`
	BackgroundRemoval bool `json:"background_removal"`
	Realtime          bool `json:"realtime"`
	MCP               bool `json:"mcp"`
	AIProviders       bool `json:"ai_providers"`
}

type configExports struct {
	FilenameStyle     string `json:"filename_style"`
	FilenameTimestamp bool   `json:"filename_timestamp"`
}

type configLimits struct {
	MaxUploadBytes          int `json:"max_upload_bytes"`
	MaxPages                int `json:"max_pages"`
	MaxDocumentBytes        int `json:"max_document_bytes"`
	RealtimeMaxParticipants int `json:"realtime_max_participants"`
	BulkFillMaxRows         int `json:"bulk_fill_max_rows"`
}

type configAssets struct {
	BackgroundRemovalModel *mlModelDescription `json:"background_removal_model"`
}

// aiConfig ports apps/ai/catalog.ai_config(): user providers mode, the
// stripped offsite notice, and per-task options.
type aiConfig struct {
	UserProviders     string        `json:"user_providers"`
	OffsiteNotice     string        `json:"offsite_notice"`
	BackgroundRemoval aiTaskOptions `json:"background_removal"`
}

type aiTaskOptions struct {
	Default *string          `json:"default"`
	Models  []aiOptionPublic `json:"models"`
}

// aiOptionPublic ports Option.public() (key order: id, label, task, kind,
// provider_name, owner, sends_offsite).
type aiOptionPublic struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Task         string `json:"task"`
	Kind         string `json:"kind"`
	ProviderName string `json:"provider_name"`
	Owner        string `json:"owner"`
	SendsOffsite bool   `json:"sends_offsite"`
}

// config ports apps/core/views.py config(). Probe 0001 step 02.
func (a *App) config(w http.ResponseWriter, r *http.Request) {
	s := a.Settings
	bg := backgroundRemovalModel(s)

	// features.background_removal == (background_removal_model() is not None)
	// features.ai_providers == has_server_options(): AI_PROVIDERS_ENABLED and
	// (env providers || enabled operator rows in db || user mode != off).
	// Env providers come later (ai family); today: none configured -> false
	// unless the DB has operator providers or user mode is on.
	aiProviders := false
	if s.AIProvidersEnabled {
		hasOperator, err := a.hasOperatorProviders()
		if err != nil {
			// The oracle surfaces db failures as 500s (handler500).
			writeDRF(w, http.StatusInternalServerError, "server_error",
				"Something went wrong on our side.")
			return
		}
		aiProviders = s.UserProvidersMode() != "off" || hasOperator
	}

	// public_options("background-removal"): device iff the model is
	// installed; env/db provider options need the ai family (none exist on a
	// fresh install, which is what scenarios assert).
	// Django initializes this to an empty list, never null: keep a
	// non-nil slice so the JSON matches byte-for-byte.
	models := make([]aiOptionPublic, 0)
	if bg != nil {
		models = append(models, aiOptionPublic{
			ID: "device", Label: "On this device", Task: "background-removal",
			Kind: "device", Owner: "device",
		})
	}

	// default_option: AI_TASK_DEFAULTS["background-removal"] (env
	// BACKGROUND_REMOVAL_DEFAULT, default "device"), else first, else null.
	var defaultID *string
	wanted := a.Settings.BackgroundRemovalDefault
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if len(ids) > 0 {
		found := ""
		for _, id := range ids {
			if id == wanted {
				found = id
				break
			}
		}
		if found == "" {
			found = ids[0]
		}
		defaultID = &found
	}

	resp := configResponse{
		GoogleClientID: s.GoogleClientID,
		Features: configFeatures{
			StockPhotos:       s.StockPhotosConfigured(),
			ServerPDF:         s.ServerPDFEnabled,
			GoogleLogin:       s.GoogleClientID != "",
			EmailVerification: s.EmailDelivers(),
			BackgroundRemoval: bg != nil,
			Realtime:          s.RealtimeEnabled,
			MCP:               s.MCPEnabled,
			AIProviders:       aiProviders,
		},
		Exports: configExports{FilenameStyle: s.ExportFilenameStyle, FilenameTimestamp: s.ExportFilenameTS},
		Limits: configLimits{
			MaxUploadBytes:          MaxUploadBytes,
			MaxPages:                MaxDesignPages,
			MaxDocumentBytes:        MaxDesignDocumentBytes,
			RealtimeMaxParticipants: s.RealtimeMaxConns,
			BulkFillMaxRows:         s.BulkFillMaxRows,
		},
		Assets: configAssets{BackgroundRemovalModel: bg},
		AI: aiConfig{
			UserProviders: s.UserProvidersMode(),
			OffsiteNotice: s.AIOffsiteNotice,
			BackgroundRemoval: aiTaskOptions{
				Default: defaultID,
				Models:  models,
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	// @cache_control(public=True, max-age=60)
	w.Header().Set("Cache-Control", "public, max-age=60")
	body := encodeCompactJSON(resp)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	_, _ = w.Write(body)
}

const (
	csrfCookieName  = "csrftoken"
	csrfSecretLen   = 32 // django/middleware/csrf.py CSRF_SECRET_LENGTH
	csrfAllowedChar = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	csrfCookieAge   = 60 * 60 * 24 * 7 * 52 // global_settings CSRF_COOKIE_AGE (31449600)
)

// csrf ports accounts CsrfView + django.middleware.csrf.get_token: the
// cookie always rides the response (renewing expiry with the SAME value when
// the client already sent a valid one), and the response varies on Cookie.
// Probe 0001 step 03 (fresh cookie; the reuse path follows get_token's
// CSRF_COOKIE_NEEDS_UPDATE behavior and is asserted by scenario step 07).
func (a *App) csrf(w http.ResponseWriter, r *http.Request) {
	secret := ""
	if c, err := r.Cookie(csrfCookieName); err == nil && validCSRFSecret(c.Value) {
		secret = c.Value // get_token: reuse and re-send to renew expiry
	} else {
		secret = newCSRFSecret()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    secret,
		Path:     "/",
		MaxAge:   csrfCookieAge,
		Expires:  time.Now().UTC().Add(csrfCookieAge * time.Second),
		SameSite: http.SameSiteLaxMode,
		Secure:   a.Settings.SessionCookieSecure(),
	})
	patchVary(w, "Cookie") // _set_csrf_cookie patches Vary: Cookie
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

func validCSRFSecret(v string) bool {
	if len(v) != csrfSecretLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		if !containsRune(csrfAllowedChar, v[i]) {
			return false
		}
	}
	return true
}

func newCSRFSecret() string {
	out := make([]byte, csrfSecretLen)
	buf := make([]byte, csrfSecretLen)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is fatal upstream too (get_random_string)
	}
	for i, b := range buf {
		out[i] = csrfAllowedChar[int(b)%len(csrfAllowedChar)]
	}
	return string(out)
}

func containsRune(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}

// hasOperatorProviders ports
// Provider.objects.filter(owner__isnull=True, enabled=True).exists()
// (apps/ai/catalog.py has_server_options) — operator-owned rows only.
func (a *App) hasOperatorProviders() (bool, error) {
	var n int
	if err := a.DB.QueryRow(
		`SELECT COUNT(*) FROM ai_provider WHERE owner_id IS NULL AND enabled`,
	).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}
