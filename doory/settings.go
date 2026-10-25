// Package doory is the Go port of the doory project settings + HTTP kernel.
//
// Settings mirror backend/doory/settings/base.py env reads 1:1 (same env
// names, same defaults, same falsy handling). Settings that upstream sets in
// settings files only (not env reads) are NOT read from env here either:
// honoring an env var upstream ignores would be a behavioral divergence.
//
// Ports backend/doory/settings/base.py
package doory

import (
	"os"
	"strconv"
	"strings"
)

// Settings is the subset of the Django settings the Go port reads, keyed to
// the config/kernel behavior it drives. Grow it as families need it.
type Settings struct {
	// Env contract (base.py env reads).
	GoogleClientID     string
	Debug              bool   // DJANGO_DEBUG (base.py env, default FALSE; dev.py sets it in-process)
	OIDCIssuer         string // OIDC_ISSUER (base.py env, default "")
	OIDCClientID       string // OIDC_CLIENT_ID
	OIDCClientSecret   string // OIDC_CLIENT_SECRET
	OIDCButtonLabel    string // OIDC_BUTTON_LABEL (default "Sign in with SSO")
	OIDCButtonIcon     string // OIDC_BUTTON_ICON (default "")
	OIDCScopes         string // OIDC_SCOPES (default "openid email profile")
	OIDCTrustEmail     bool   // OIDC_TRUST_EMAIL (default false)
	UnsplashAccessKey  string
	PexelsAccessKey    string
	PixabayAccessKey   string
	FreesoundAccessKey string
	// Stock videos (0.2.0): their own keys, falling back to the photo key
	// of the same provider; listed in STOCK_VIDEO_PROVIDERS tab order.
	PexelsVideoAPIKey   string
	PixabayVideoAPIKey  string
	StockVideoProviders []string
	// Video clips: on when VIDEO_ENABLED and both commands exist; the
	// upload/stock endpoints report availability and limits from here.
	VideoEnabled            bool
	FFmpegPath              string
	FFprobePath             string
	MaxVideoUploadBytes     int
	MaxVideoDurationSeconds int
	// Transactional email (base.py EMAIL_URL/EMAIL_SEND_ASYNC +
	// AUTH_EMAIL_COOLDOWN_SECONDS); D9 for the template rendering.
	EmailURL                 string
	EmailSendAsync           bool
	AuthEmailCooldownSeconds int
	ServerPDFEnabled         bool
	ExportFilenameStyle      string
	ExportFilenameTS         bool
	MediaRoot                string
	BulkFillMaxRows          int
	MCPEnabled               bool
	McpPath                  string // MCP_PATH (default /mcp)
	AIUserProviders          string
	AIOffsiteNotice          string
	AIProvidersEnabled       bool
	StockProviders           []string
	BackgroundRemovalPin     string
	RealtimeEnabled          bool
	RealtimeMaxConns         int
	SiteURL                  string
	DatabaseURL              string
	// DevProxyURL is `doory dev`'s Vite dev server (M14): every non-API path
	// is proxied there, mirroring frontend/vite.config.ts's server.proxy.
	DevProxyURL              string
	AllowedOrigins           []string // CORS_ALLOWED_ORIGINS (prod.py env.list, default none)
	SecureCookies            bool     // SECURE_COOKIES (prod.py env read, default true): session+csrf cookie Secure attr
	SecretKey                string   // DJANGO_SECRET_KEY (base.py env read, default dev-insecure...)
	SecretKeyFallbacks       []string // SECRET_KEY_FALLBACKS (base.py env.list): rotated-away keys
	MaxAPITokensPerUser      int      // MAX_API_TOKENS_PER_USER (base.py env, default 20)
	MaxDesignsPerUser        int      // MAX_DESIGNS_PER_USER (base.py env, default 5000)
	UserStorageQuotaBytes    int64    // USER_STORAGE_QUOTA_BYTES (base.py env, default 2GiB)
	ServerImageExportEnabled bool     // SERVER_IMAGE_EXPORT_ENABLED (base.py env, default FALSE)
	ExportRenderPageURL      string   // EXPORT_RENDER_PAGE_URL (default: SITE_URL origin + /render.html)
	ExportImageMaxPixels     int      // EXPORT_IMAGE_MAX_PIXELS (default 40M)
	ExportImageMaxDocBytes   int      // EXPORT_IMAGE_MAX_DOCUMENT_BYTES (default 5MiB)
	ExportMaxConcurrency     int      // EXPORT_MAX_CONCURRENCY (default 2)
	ExportTimeoutSeconds     float64  // EXPORT_TIMEOUT_SECONDS (default 30)
	ExportChromiumPath       string   // EXPORT_CHROMIUM_PATH (empty: PATH lookup)
	ExportChromiumNoSandbox  bool     // EXPORT_CHROMIUM_NO_SANDBOX (default false)
	ExportWorkDir            string   // EXPORT_WORK_DIR (default os temp)
	ExportAllowedOrigins     []string // EXPORT_ALLOWED_ORIGINS (default site origin + :8111)
	ExportStorageRoot        string   // EXPORT_STORAGE_ROOT (default ./exports)
	ImageOptimizers          []string // IMAGE_OPTIMIZERS (base.py env.list, default ["builtin"])
	ImageOptimizerDefault    string   // IMAGE_OPTIMIZER_DEFAULT (default "builtin")
	TinyPngAPIKey            string   // TINYPNG_API_KEY
	KrakenAPIKey             string   // KRAKEN_API_KEY
	KrakenAPISecret          string   // KRAKEN_API_SECRET
	ImageOptimizerCustomName string   // IMAGE_OPTIMIZER_CUSTOM_NAME (default "Custom")
	ImageOptimizerCustomURL  string   // IMAGE_OPTIMIZER_CUSTOM_URL (default "")
	ImageOptimizerCustomKey  string   // IMAGE_OPTIMIZER_CUSTOM_KEY (default "")
	ImageOptimizerMaxBytes   int      // IMAGE_OPTIMIZER_MAX_BYTES (default 25 MiB)
	McpExportURLTTLSeconds   int      // MCP_EXPORT_URL_TTL_SECONDS (default 600)
	McpCallsPerMinute        int      // MCP_CALLS_PER_MINUTE (default 120; 0 = unlimited)
	McpMaxRequestBytes       int      // MCP_MAX_REQUEST_BYTES (default 4MB)
	McpExportInlineMaxBytes  int      // MCP_EXPORT_INLINE_MAX_BYTES (default 1MB)
	McpExportURLUploads      bool     // URL_IMAGE_UPLOADS (default true; MCP_URL_UPLOADS legacy)
	BackgroundRemovalDefault string   // AI_TASK_DEFAULTS[background-removal] (base.py env, default "device")
	AIProviderEnvProvider    []AIEnvProvider

	// AI knobs (base.py env reads, M10).
	AIUserProvidersAllowHTTP    bool     // AI_USER_PROVIDERS_ALLOW_HTTP
	AIUserProvidersAllowedPorts []int    // AI_USER_PROVIDERS_ALLOWED_PORTS
	AIAllowedPrivateHosts       []string // AI_ALLOWED_PRIVATE_HOSTS
	AISecretsKey                string   // AI_SECRETS_KEY (empty: derive from SECRET_KEY)
	AISecretsKeyFallbacks       []string // AI_SECRETS_KEY_FALLBACKS
	AllowedHosts                []string // DJANGO_ALLOWED_HOSTS (handshake origin validation)
	CSRFTrustedOrigins          []string // CSRF_TRUSTED_ORIGINS

	// Not env-read upstream: Django's own default (base.py never sets
	// EMAIL_BACKEND; Django defaults to SMTP; dev/test files override it
	// in-process). Kept as a constant field so the email_delivers rule
	// (apps/core/views.py) is implementable without inventing an env var.
	EmailBackend string
}

// AIEnvProvider mirrors one AI_PROVIDER_<n>_* row from base.py (env-driven
// operator provider declarations; field list AI_ENV_PROVIDER_FIELDS).
type AIEnvProvider struct {
	index           string
	URL             string
	STYLE           string
	TASKS           string
	MODELS          string
	AUTH_HEADER     string
	AUTH_VALUE      string
	STYLE_CONFIG    string
	LABEL           string
	SENDS_OFFSITE   string
	TIMEOUT_SECONDS string
	MAX_IMAGE_BYTES string
	DAILY_QUOTA     string
}

// DjangoNonDeliveringEmailBackends is the set in apps/core/views.py
// email_delivers(): backends that never deliver, so users can't be asked to
// verify an address.
var DjangoNonDeliveringEmailBackends = map[string]bool{
	"django.core.mail.backends.console.EmailBackend":   true,
	"django.core.mail.backends.dummy.EmailBackend":     true,
	"django.core.mail.backends.filebased.EmailBackend": true,
	"django.core.mail.backends.locmem.EmailBackend":    true,
}

// EmailDelivers ports email_delivers(): whether verification emails reach
// anyone.
func (s *Settings) EmailDelivers() bool {
	return !DjangoNonDeliveringEmailBackends[s.EmailBackend]
}

// FromEnv builds Settings from the process environment using the same
// names/defaults as base.py. Missing vars take the upstream defaults.
func FromEnv() *Settings {
	settings := &Settings{
		GoogleClientID:              envStr("GOOGLE_CLIENT_ID", ""),
		Debug:                       envBool("DJANGO_DEBUG", false),
		OIDCIssuer:                  envStr("OIDC_ISSUER", ""),
		OIDCClientID:                envStr("OIDC_CLIENT_ID", ""),
		OIDCClientSecret:            envStr("OIDC_CLIENT_SECRET", ""),
		OIDCButtonLabel:             envStr("OIDC_BUTTON_LABEL", "Sign in with SSO"),
		OIDCButtonIcon:              envStr("OIDC_BUTTON_ICON", ""),
		OIDCScopes:                  envStr("OIDC_SCOPES", "openid email profile"),
		OIDCTrustEmail:              envBool("OIDC_TRUST_EMAIL", false),
		UnsplashAccessKey:           envStr("UNSPLASH_ACCESS_KEY", ""),
		PexelsAccessKey:             envStr("PEXELS_API_KEY", ""),
		PixabayAccessKey:            envStr("PIXABAY_API_KEY", ""),
		FreesoundAccessKey:          envStr("FREESOUND_ACCESS_KEY", ""),
		PexelsVideoAPIKey:           envStr("PEXELS_VIDEO_API_KEY", ""),
		PixabayVideoAPIKey:          envStr("PIXABAY_VIDEO_API_KEY", ""),
		StockVideoProviders:         envList("STOCK_VIDEO_PROVIDERS", []string{"pexels_video", "pixabay_video"}),
		VideoEnabled:                envBool("VIDEO_ENABLED", true),
		FFmpegPath:                  envStr("FFMPEG_PATH", "ffmpeg"),
		FFprobePath:                 envStr("FFPROBE_PATH", "ffprobe"),
		MaxVideoUploadBytes:         envInt("MAX_VIDEO_UPLOAD_BYTES", 200*1024*1024),
		MaxVideoDurationSeconds:     envInt("MAX_VIDEO_DURATION_SECONDS", 600),
		EmailURL:                    envStr("EMAIL_URL", "consolemail://"),
		EmailSendAsync:              envBool("EMAIL_SEND_ASYNC", true),
		AuthEmailCooldownSeconds:    envInt("AUTH_EMAIL_COOLDOWN_SECONDS", 60),
		ServerPDFEnabled:            envBool("SERVER_PDF_ENABLED", true),
		ExportFilenameStyle:         envStr("EXPORT_FILENAME_STYLE", "simple"),
		ExportFilenameTS:            envBool("EXPORT_FILENAME_TIMESTAMP", false),
		MediaRoot:                   envStr("MEDIA_ROOT", defaultDataPath("media")),
		BulkFillMaxRows:             envInt("BULK_FILL_MAX_ROWS", 100),
		MCPEnabled:                  envBool("MCP_ENABLED", false),
		McpPath:                     envStr("MCP_PATH", "/mcp"),
		AIUserProviders:             envStr("AI_USER_PROVIDERS", "off"),
		AIOffsiteNotice:             strings.TrimSpace(envStr("AI_OFFSITE_NOTICE", "")),
		AIProvidersEnabled:          envBool("AI_PROVIDERS_ENABLED", true),
		AIUserProvidersAllowHTTP:    envBool("AI_USER_PROVIDERS_ALLOW_HTTP", false),
		AIUserProvidersAllowedPorts: envIntList("AI_USER_PROVIDERS_ALLOWED_PORTS"),
		AIAllowedPrivateHosts:       envList("AI_ALLOWED_PRIVATE_HOSTS", nil),
		AISecretsKey:                envStr("AI_SECRETS_KEY", ""),
		AISecretsKeyFallbacks:       envList("AI_SECRETS_KEY_FALLBACKS", nil),
		AllowedHosts:                envList("DJANGO_ALLOWED_HOSTS", []string{"localhost", "127.0.0.1"}),
		CSRFTrustedOrigins:          envList("CSRF_TRUSTED_ORIGINS", nil),
		StockProviders:              envList("STOCK_PROVIDERS", []string{"unsplash", "pexels", "pixabay"}),
		BackgroundRemovalPin:        envStr("BACKGROUND_REMOVAL_MODEL", ""),
		RealtimeEnabled:             envBool("REALTIME_ENABLED", true),
		RealtimeMaxConns:            envInt("REALTIME_MAX_CONNECTIONS_PER_DESIGN", 25),
		SiteURL:                     envStr("SITE_URL", "http://localhost:5173"),
		DatabaseURL:                 envStr("DATABASE_URL", defaultDatabaseURL()),
		DevProxyURL:                 envStr("DOORY_DEV_PROXY", ""),
		AllowedOrigins:              envList("CORS_ALLOWED_ORIGINS", nil),
		SecureCookies:               envBool("SECURE_COOKIES", true),
		SecretKey:                   envStr("DJANGO_SECRET_KEY", "dev-insecure-secret-key-change-me"),
		SecretKeyFallbacks:          envList("SECRET_KEY_FALLBACKS", nil),
		MaxAPITokensPerUser:         envInt("MAX_API_TOKENS_PER_USER", 20),
		ServerImageExportEnabled:    envBool("SERVER_IMAGE_EXPORT_ENABLED", false),
		ExportImageMaxPixels:        envInt("EXPORT_IMAGE_MAX_PIXELS", 40_000_000),
		ExportImageMaxDocBytes:      envInt("EXPORT_IMAGE_MAX_DOCUMENT_BYTES", 5*1024*1024),
		ExportMaxConcurrency:        envInt("EXPORT_MAX_CONCURRENCY", 2),
		ExportTimeoutSeconds:        float64(envInt("EXPORT_TIMEOUT_SECONDS", 30)),
		ExportChromiumPath:          envStr("EXPORT_CHROMIUM_PATH", ""),
		ExportRenderPageURL:         envStr("EXPORT_RENDER_PAGE_URL", ""),
		ExportAllowedOrigins:        envList("EXPORT_ALLOWED_ORIGINS", nil),
		ExportChromiumNoSandbox:     envBool("EXPORT_CHROMIUM_NO_SANDBOX", false),
		ExportWorkDir:               envStr("EXPORT_WORK_DIR", ""),
		ExportStorageRoot:           envStr("EXPORT_STORAGE_ROOT", defaultDataPath("exports")),
		ImageOptimizers:             envList("IMAGE_OPTIMIZERS", []string{"builtin"}),
		ImageOptimizerDefault:       envStr("IMAGE_OPTIMIZER_DEFAULT", "builtin"),
		TinyPngAPIKey:               envStr("TINYPNG_API_KEY", ""),
		KrakenAPIKey:                envStr("KRAKEN_API_KEY", ""),
		KrakenAPISecret:             envStr("KRAKEN_API_SECRET", ""),
		ImageOptimizerCustomName:    envStr("IMAGE_OPTIMIZER_CUSTOM_NAME", "Custom"),
		ImageOptimizerCustomURL:     envStr("IMAGE_OPTIMIZER_CUSTOM_URL", ""),
		ImageOptimizerCustomKey:     envStr("IMAGE_OPTIMIZER_CUSTOM_KEY", ""),
		ImageOptimizerMaxBytes:      envInt("IMAGE_OPTIMIZER_MAX_BYTES", 25*1024*1024),
		McpExportURLTTLSeconds:      envInt("MCP_EXPORT_URL_TTL_SECONDS", 600),
		McpCallsPerMinute:           envInt("MCP_CALLS_PER_MINUTE", 120),
		McpMaxRequestBytes:          envInt("MCP_MAX_REQUEST_BYTES", 4*1024*1024),
		McpExportInlineMaxBytes:     envInt("MCP_EXPORT_INLINE_MAX_BYTES", 1024*1024),
		McpExportURLUploads:         envBool("URL_IMAGE_UPLOADS", true),
		MaxDesignsPerUser:           envInt("MAX_DESIGNS_PER_USER", 5000),
		UserStorageQuotaBytes:       int64(envInt("USER_STORAGE_QUOTA_BYTES", 2*1024*1024*1024)),
		BackgroundRemovalDefault:    envStr("BACKGROUND_REMOVAL_DEFAULT", "device"),
		// base.py hardcodes EMAIL_BACKEND = console (a settings-file
		// constant, NOT an env read): upstream's deployed default never
		// delivers mail, so features.email_verification is false on a
		// fresh install everywhere — probed: prod settings resolve to
		// console too (smtp exists only in Django's global_settings,
		// which base.py overrides).
		EmailBackend: "django.core.mail.backends.console.EmailBackend",
	}
	if parsed := parseEmailURL(settings.EmailURL); parsed.Backend == "smtp" {
		settings.EmailBackend = "django.core.mail.backends.smtp.EmailBackend"
	}
	settings.applyExportDefaults()
	if len(settings.CSRFTrustedOrigins) == 0 {
		// prod.py: CSRF_TRUSTED_ORIGINS defaults to SITE_URL when unset.
		settings.CSRFTrustedOrigins = []string{settings.SiteURL}
	}
	settings.AIProviderEnvProvider = loadAIEnvProviderRows()
	return settings
}

// loadAIEnvProviderRows mirrors base.py's AI_ENV_PROVIDERS comprehension:
// slots 1..9 with a URL are kept, each field read from AI_PROVIDER_<n>_<FIELD>.
func loadAIEnvProviderRows() []AIEnvProvider {
	var rows []AIEnvProvider
	for n := 1; n <= 9; n++ {
		idx := strconv.Itoa(n)
		if envStr("AI_PROVIDER_"+idx+"_URL", "") == "" {
			continue
		}
		rows = append(rows, AIEnvProvider{
			index:           idx,
			URL:             envStr("AI_PROVIDER_"+idx+"_URL", ""),
			STYLE:           envStr("AI_PROVIDER_"+idx+"_STYLE", ""),
			TASKS:           envStr("AI_PROVIDER_"+idx+"_TASKS", ""),
			MODELS:          envStr("AI_PROVIDER_"+idx+"_MODELS", ""),
			AUTH_HEADER:     envStr("AI_PROVIDER_"+idx+"_AUTH_HEADER", ""),
			AUTH_VALUE:      envStr("AI_PROVIDER_"+idx+"_AUTH_VALUE", ""),
			STYLE_CONFIG:    envStr("AI_PROVIDER_"+idx+"_STYLE_CONFIG", ""),
			LABEL:           envStr("AI_PROVIDER_"+idx+"_LABEL", ""),
			SENDS_OFFSITE:   envStr("AI_PROVIDER_"+idx+"_SENDS_OFFSITE", ""),
			TIMEOUT_SECONDS: envStr("AI_PROVIDER_"+idx+"_TIMEOUT_SECONDS", ""),
			MAX_IMAGE_BYTES: envStr("AI_PROVIDER_"+idx+"_MAX_IMAGE_BYTES", ""),
			DAILY_QUOTA:     envStr("AI_PROVIDER_"+idx+"_DAILY_QUOTA", ""),
		})
	}
	return rows
}

// exportDefaults computes the SITE_URL-derived export settings (mirroring
// base.py: render page URL and allowed origins).
func (s *Settings) applyExportDefaults() {
	if s.ExportRenderPageURL == "" {
		s.ExportRenderPageURL = originOf(s.SiteURL) + "/render.html"
	}
	if len(s.ExportAllowedOrigins) == 0 {
		s.ExportAllowedOrigins = []string{originOf(s.SiteURL), "http://127.0.0.1:8111", "http://localhost:8111"}
	}
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

// SessionCookieSecure: prod.py sets SESSION_COOKIE_SECURE =
// CSRF_COOKIE_SECURE = env.bool("SECURE_COOKIES", default True). The plain-
// HTTP probe/scenario configuration passes SECURE_COOKIES=false on both
// sides (documented prod.py opt-out).
func (s *Settings) SessionCookieSecure() bool { return s.SecureCookies }

// UserProvidersMode ports apps/ai/catalog.user_providers_mode(): invalid
// values fall back to "off".
func (s *Settings) UserProvidersMode() string {
	mode := strings.ToLower(strings.TrimSpace(s.AIUserProviders))
	for _, m := range []string{"off", "staff", "everyone"} {
		if mode == m {
			return mode
		}
	}
	return "off"
}

// StockPhotosConfigured ports the `configured` half of
// apps/assets/providers.py configured_providers() for photo providers:
// a provider is on when it has a key and STOCK_PROVIDERS lists it. The
// assets family owns the full port; config only needs the feature flag.
func (s *Settings) StockPhotosConfigured() bool {
	keys := map[string]string{
		"unsplash": s.UnsplashAccessKey,
		"pexels":   s.PexelsAccessKey,
		"pixabay":  s.PixabayAccessKey,
	}
	for _, name := range s.StockProviders {
		name = strings.ToLower(strings.TrimSpace(name))
		if key, ok := keys[name]; ok && key != "" {
			return true
		}
	}
	return false
}

// Fixed upstream constants used by /config/ (settings file constants, not
// env reads — see doory/settings/base.py:124-130).
const (
	MaxUploadBytes         = 25 * 1024 * 1024 // base.py MAX_UPLOAD_BYTES
	MaxDesignDocumentBytes = 12 * 1024 * 1024 // base.py MAX_DESIGN_DOCUMENT_BYTES
	MaxDesignPages         = 100              // base.py MAX_DESIGN_PAGES
)

// env helpers: same coercions as django-environ (bool accepts 1/true/yes/on).

// dataDir is the port's data directory (D4): everything a self-hosted install
// needs to keep — the SQLite database, media and exports — lives under one
// directory that can be mounted as a volume. DOORY_DATA_DIR overrides it;
// the default is ./data next to the binary's working directory.
func dataDir() string { return envStr("DOORY_DATA_DIR", "data") }

// defaultDataPath is one path under the data directory.
func defaultDataPath(name string) string { return dataDir() + "/" + name }

// defaultDatabaseURL is the no-config default (D4): an embedded SQLite file in
// the data directory. The parent directory is created on open.
func defaultDatabaseURL() string { return "sqlite:///" + defaultDataPath("doory.sqlite3") }

func envStr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envBool(name string, def bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "t", "on":
		return true
	case "0", "false", "no", "n", "f", "off":
		return false
	}
	return def
}

func envInt(name string, def int) int {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envList(name string, def []string) []string {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// envIntList parses a comma-separated int list (base.py env.list(cast=int)).
func envIntList(name string) []int {
	v, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	var out []int
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// envFloat parses a float env var (base.py env.float).
func envFloat(name string, def float64) float64 {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

// emailConfig parses EMAIL_URL into the transport the sender uses.
func (s *Settings) emailConfig() emailConfig { return parseEmailURL(s.EmailURL) }
