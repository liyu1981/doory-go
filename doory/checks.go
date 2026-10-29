// Django's system check framework, as far as the ported settings surface
// registers checks: apps/exports/checks.py (IMAGE_OPTIMIZERS warnings) and
// apps/accounts/checks.py (OpenID Connect settings). Django runs them before
// every management command (requires_system_checks = '__all__') and prints
// the warnings to stderr in base.py's format:
//
//	System check identified some issues:
//
//	WARNINGS:
//	?: (exports.W001) MESSAGE
//		HINT: HINT
//
// The `-h/--help` shapes (`manage.py check`) and the remaining check modules
// (deployment checks, database checks) are unported; PORT_MAP tracks them.
package doory

import (
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
)

// systemCheckWarning is one django.core.checks.Warning, everything the
// printed form needs (Issue.__str__: "?: (id) msg\n\tHINT: hint").
type systemCheckWarning struct {
	ID      string
	Message string
	Hint    string
}

func (w systemCheckWarning) String() string {
	hint := ""
	if w.Hint != "" {
		hint = "\n\tHINT: " + w.Hint
	}
	return fmt.Sprintf("?: (%s) %s%s", w.ID, w.Message, hint)
}

// systemCheckWarnings ports the registered checks for the pinned oracle's
// default configuration. Order follows registration (apps in INSTALLED_APPS
// order); the caller sorts within the WARNINGS group like Django.
func (a *App) systemCheckWarnings() []systemCheckWarning {
	var out []systemCheckWarning
	out = append(out, a.accountsChecks()...)
	out = append(out, a.exportsChecks()...)
	return out
}

// accountsChecks ports apps/accounts/checks.py: configuration_problems()
// turns each problem into one accounts.W001 warning.
func (a *App) accountsChecks() []systemCheckWarning {
	s := a.Settings
	var out []systemCheckWarning
	for _, problem := range s.oidcConfigurationProblems() {
		out = append(out, systemCheckWarning{
			ID:      "accounts.W001",
			Message: problem + " OpenID Connect sign-in is off.",
			Hint:    "See OpenID Connect sign-in in docs/self-hosting/configuration.md.",
		})
	}
	return out
}

// oidcRequiredSettings mirrors oidc.REQUIRED_SETTINGS order.
var oidcRequiredSettings = []struct {
	name  string
	value func(*Settings) string
}{
	{"OIDC_ISSUER", func(s *Settings) string { return s.OIDCIssuer }},
	{"OIDC_CLIENT_ID", func(s *Settings) string { return s.OIDCClientID }},
	{"OIDC_CLIENT_SECRET", func(s *Settings) string { return s.OIDCClientSecret }},
}

const maxOIDCIssuerLength = 255 // oidc.MAX_ISSUER_LENGTH

// oidcConfigurationProblems ports oidc.configuration_problems().
func (s *Settings) oidcConfigurationProblems() []string {
	any := false
	for _, setting := range oidcRequiredSettings {
		if setting.value(s) != "" {
			any = true
			break
		}
	}
	if !any {
		return nil
	}
	var problems []string
	for _, setting := range oidcRequiredSettings {
		if setting.value(s) == "" {
			problems = append(problems, setting.name+" is not set.")
		}
	}
	issuer := s.OIDCIssuer
	if issuer != "" && !s.oidcAllowedURL(issuer, false) {
		problems = append(problems, fmt.Sprintf(
			"OIDC_ISSUER=%s is not an https:// URL without a query or fragment "+
				"(http:// works only with DEBUG on).", pyRepr(issuer)))
	} else if len(strings.TrimRight(issuer, "/")) >= maxOIDCIssuerLength {
		problems = append(problems, fmt.Sprintf("OIDC_ISSUER is longer than %d characters.", maxOIDCIssuerLength-1))
	}
	if !containsWord(s.OIDCScopes, "openid") {
		problems = append(problems, "OIDC_SCOPES must include openid.")
	}
	if !s.oidcAllowedURL(s.SiteURL, false) {
		problems = append(problems, fmt.Sprintf(
			"SITE_URL=%s must be https://: the provider sends people there.", pyRepr(s.SiteURL)))
	}
	return problems
}

// oidcAllowedURL ports oidc._allowed_url: an absolute https:// URL (http://
// too with DEBUG on), no fragment, and no query unless `query`.
func (s *Settings) oidcAllowedURL(raw string, query bool) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && !(s.Debug && scheme == "http") {
		return false
	}
	return parsed.Hostname() != "" && parsed.Fragment == "" &&
		(query || parsed.RawQuery == "")
}

// containsWord ports Python's `word in value.split()`.
func containsWord(value, word string) bool {
	for _, field := range strings.Fields(value) {
		if field == word {
			return true
		}
	}
	return false
}

// optimizerIDs mirrors optimizers.OPTIMIZERS registration order.
var optimizerIDs = []string{"builtin", "tinypng", "kraken", "custom"}

// exportsChecks ports apps/exports/checks.py:check_image_optimizers.
func (a *App) exportsChecks() []systemCheckWarning {
	s := a.Settings
	var out []systemCheckWarning

	names := make([]string, 0, len(s.ImageOptimizers))
	for _, raw := range s.ImageOptimizers {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name != "" {
			names = append(names, name)
		}
	}
	known := map[string]bool{}
	for _, id := range optimizerIDs {
		known[id] = true
	}
	for _, name := range names {
		if !known[name] {
			out = append(out, systemCheckWarning{
				ID:      "exports.W001",
				Message: fmt.Sprintf("IMAGE_OPTIMIZERS: unknown optimizer %s; it is left out.", pyRepr(name)),
				Hint:    "Use " + strings.Join(optimizerIDs, ", ") + ".",
			})
		}
	}

	listed := []string{}
	seen := map[string]bool{}
	for _, name := range names {
		if known[name] && !seen[name] {
			seen[name] = true
			listed = append(listed, name)
		}
	}
	hasListed := func(want string) bool {
		for _, name := range listed {
			if name == want {
				return true
			}
		}
		return false
	}

	needs := []struct {
		name string
		odd  string
		ok   bool
	}{
		{"tinypng", "TINYPNG_API_KEY", strings.TrimSpace(s.TinyPngAPIKey) != ""},
		{"kraken", "KRAKEN_API_KEY and KRAKEN_API_SECRET",
			strings.TrimSpace(s.KrakenAPIKey) != "" && strings.TrimSpace(s.KrakenAPISecret) != ""},
	}
	for _, entry := range needs {
		if hasListed(entry.name) && !entry.ok {
			out = append(out, systemCheckWarning{
				ID: "exports.W003",
				Message: fmt.Sprintf("IMAGE_OPTIMIZERS lists %s but %s is not set; it is left out.",
					entry.name, entry.odd),
			})
		}
	}

	if hasListed("custom") {
		raw := strings.TrimSpace(s.ImageOptimizerCustomURL)
		problem := "it is not set"
		if raw != "" {
			if msg := s.customOptimizerURLProblem(raw); msg != "" {
				problem = msg
			} else {
				problem = customOptimizerKeyProblem(s.ImageOptimizerCustomKey)
			}
		}
		if problem != "" {
			out = append(out, systemCheckWarning{
				ID: "exports.W003",
				Message: "IMAGE_OPTIMIZER_CUSTOM_URL: " + strings.TrimRight(problem, ".") +
					"; the custom optimizer is left out.",
			})
		} else if strings.TrimSpace(s.ImageOptimizerCustomKey) != "" &&
			strings.ToLower(urlScheme(raw)) == "http" {
			out = append(out, systemCheckWarning{
				ID: "exports.W005",
				Message: "IMAGE_OPTIMIZER_CUSTOM_URL is a plain http:// address: " +
					"IMAGE_OPTIMIZER_CUSTOM_KEY and the images are sent to it unencrypted.",
				Hint: "Use an https:// address, or no key for an optimizer on your own private network.",
			})
		}
	}

	def := strings.ToLower(strings.TrimSpace(s.ImageOptimizerDefault))
	if def != "" && len(listed) > 0 && !hasListed(def) {
		out = append(out, systemCheckWarning{
			ID: "exports.W004",
			Message: fmt.Sprintf("IMAGE_OPTIMIZER_DEFAULT=%s is not in IMAGE_OPTIMIZERS; "+
				"exports with optimize=true are not optimized.", pyRepr(def)),
			Hint: "Pick one of IMAGE_OPTIMIZERS, or leave it empty.",
		})
	}
	return out
}

// customOptimizerURLProblem ports optimizers.custom.url_problem.
func (s *Settings) customOptimizerURLProblem(raw string) string {
	if _, terr := s.aiCheckURL(raw, s.aiOperatorPolicy(), false); terr != nil {
		return terr.message
	}
	if strings.ContainsAny(raw, "?#") {
		return "The address may not have a query or fragment."
	}
	return ""
}

// customOptimizerKeyProblem ports optimizers.custom.key_problem.
func customOptimizerKeyProblem(key string) string {
	if strings.ContainsAny(key, "\r\n") {
		return "The key contains a line break."
	}
	return ""
}

func urlScheme(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Scheme
}

// printSystemChecks writes Django's warning banner (base.py check()) for the
// registered warnings, or nothing when there are none. Instructions go to
// stderr; the body is grouped and sorted exactly like the framework's.
func (a *App) printSystemChecks(w io.Writer) {
	warnings := a.systemCheckWarnings()
	if len(warnings) == 0 {
		return
	}
	formatted := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		formatted = append(formatted, warning.String())
	}
	sort.Strings(formatted)
	fmt.Fprintf(w, "System check identified some issues:\n\nWARNINGS:\n%s\n", strings.Join(formatted, "\n"))
}

// ---- /config/ helpers: apps/accounts/oidc.py and apps/exports/optimizers ----

// oidcEnabled ports oidc.enabled(): every required setting set and no
// configuration problems.
func (s *Settings) oidcEnabled() bool {
	for _, setting := range oidcRequiredSettings {
		if setting.value(s) == "" {
			return false
		}
	}
	return len(s.oidcConfigurationProblems()) == 0
}

const oidcDefaultButtonLabel = "Sign in with SSO" // oidc.DEFAULT_BUTTON_LABEL

// oidcButtonLabel ports oidc.button_label().
func (s *Settings) oidcButtonLabel() string {
	if label := strings.TrimSpace(s.OIDCButtonLabel); label != "" {
		return label
	}
	return oidcDefaultButtonLabel
}

// oidcButtonIcon ports oidc.button_icon(): the lowercased name when it
// matches ICON_NAME, else "".
func (s *Settings) oidcButtonIcon() string {
	name := strings.ToLower(strings.TrimSpace(s.OIDCButtonIcon))
	if name == "" || len(name) > 40 {
		return ""
	}
	for i, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok || (i == 0 && r == '-') {
			return ""
		}
	}
	return name
}

// imageOptimizerFormats is what this server can actually convert to:
// upstream's list without AVIF (no pure-Go AVIF encoder, D21). Optimizers
// that accept more formats report their own list (D20/D21 notes).
var imageOptimizerFormats = []string{"png", "jpg", "webp"}

// imageOptimizeConfig ports core/views.image_optimize_config() and the
// optimizer registry (available() + default_optimizer()).
func (a *App) imageOptimizeConfig() configImageOptimize {
	s := a.Settings
	available := []configImageOptimizer{}
	ids := map[string]bool{}
	for _, name := range s.optimizerListed() {
		switch name {
		case "builtin":
			// The embedded cjpeg/oxipng WASI workers are always available,
			// so builtin needs no external binaries (D22).
			available = append(available, configImageOptimizer{
				ID: "builtin", Name: "Built-in", Formats: optimizerBuiltinFormats,
			})
			ids["builtin"] = true
		case "tinypng":
			if strings.TrimSpace(s.TinyPngAPIKey) == "" {
				continue
			}
			available = append(available, configImageOptimizer{
				ID: "tinypng", Name: "TinyPNG", Formats: optimizerOffsiteFormats, SendsOffsite: true,
			})
			ids["tinypng"] = true
		case "kraken":
			if strings.TrimSpace(s.KrakenAPIKey) == "" || strings.TrimSpace(s.KrakenAPISecret) == "" {
				continue
			}
			available = append(available, configImageOptimizer{
				ID: "kraken", Name: "Kraken.io", Formats: optimizerOffsiteFormats, SendsOffsite: true,
			})
			ids["kraken"] = true
		case "custom":
			raw := strings.TrimSpace(s.ImageOptimizerCustomURL)
			if raw == "" || s.customOptimizerURLProblem(raw) != "" ||
				customOptimizerKeyProblem(s.ImageOptimizerCustomKey) != "" {
				continue
			}
			name := strings.TrimSpace(s.ImageOptimizerCustomName)
			if len([]rune(name)) > 40 {
				name = string([]rune(name)[:40])
			}
			if name == "" {
				name = "Custom"
			}
			available = append(available, configImageOptimizer{
				ID: "custom", Name: name, Formats: optimizerOffsiteFormats, SendsOffsite: true,
			})
			ids["custom"] = true
		}
	}
	var defaultID *string
	if def := strings.ToLower(strings.TrimSpace(s.ImageOptimizerDefault)); def != "" && ids[def] {
		value := def
		defaultID = &value
	}
	return configImageOptimize{
		Formats:    imageOptimizerFormats,
		Optimizers: available,
		Default:    defaultID,
	}
}

// optimizerOffsiteFormats is the contract surface the off-site optimizers
// advertise (their service accepts AVIF even though this server does not
// make it locally: the file they return is theirs).
var optimizerOffsiteFormats = []string{"png", "jpg", "webp", "avif"}

// optimizerListed ports optimizers.listed(): stripped lowercased known ids,
// in order and without repeats.
func (s *Settings) optimizerListed() []string {
	known := map[string]bool{}
	for _, id := range optimizerIDs {
		known[id] = true
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range s.ImageOptimizers {
		name := strings.ToLower(strings.TrimSpace(raw))
		if known[name] && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
