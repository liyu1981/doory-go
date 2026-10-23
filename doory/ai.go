// ai family (apps/ai): settings, secrets keys, styles, transport policy, env
// providers, option catalogue, provider rows, views and the task run path —
// M10 slice.
//
// Probed surface (probes/0010-ai, two phases): contract.yaml byte-served,
// GET /ai/models/ option shapes, unknown_task 404, the AI_USER_PROVIDERS gate
// (403 ai_user_providers_disabled), provider CRUD with every validation
// message, the keeps-secret address rule, model replace, per-user limit, and
// the run path up to the transport (ai_model_not_found / ai_unreachable —
// connection refused under the operator policy). Style pipelines beyond the
// doory multipart build, load-models/test actions, HTTP-form quotas and the
// admin site are pending slices (PLAN.md M10).
//
// Secrets at rest: internal/aikeys (Fernet + HKDF from SECRET_KEY),
// cross-proven both directions against the oracle's cryptography library.
package doory

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"doory-go/internal/aikeys"
)

// ---------------------------------------------------------------- settings ---

// AIMaxProvidersPerUser ports AI_MAX_PROVIDERS_PER_USER (default 10).
func (s *Settings) AIMaxProvidersPerUser() int { return envInt("AI_MAX_PROVIDERS_PER_USER", 10) }

// AIMaxImageBytes ports AI_MAX_IMAGE_BYTES (default 25 MiB).
func (s *Settings) AIMaxImageBytes() int64 {
	return int64(envInt("AI_MAX_IMAGE_BYTES", 25*1024*1024))
}

// userProvidersMode ports catalog.user_providers_mode (invalid values -> "off").
func (s *Settings) userProvidersMode() string {
	m := strings.ToLower(strings.TrimSpace(s.AIUserProviders))
	if m == "off" || m == "staff" || m == "everyone" {
		return m
	}
	return "off"
}

// aiSecretKeys ports apps/ai/secrets._keys(): the primary key (or the
// SECRET_KEY derivation) first, then derived fallbacks. Invalid key strings
// are skipped like MultiFernet skips a bad fallback; a bad PRIMARY key makes
// secrets unreadable/unwritable.
func (s *Settings) aiSecretKeys() (keys []string, primaryInvalid bool) {
	valid := func(k string) bool {
		if k == "" {
			return false
		}
		_, _, err := aikeys.KeyMaterial(k)
		return err == nil
	}
	if s.AISecretsKey != "" {
		if !valid(s.AISecretsKey) {
			primaryInvalid = true
		} else {
			keys = append(keys, s.AISecretsKey)
		}
		for _, fb := range s.AISecretsKeyFallbacks {
			if valid(fb) {
				keys = append(keys, fb)
			}
		}
		if k := string(aikeys.DeriveKey(s.SecretKey)); valid(k) {
			keys = append(keys, k)
		}
	} else if k := string(aikeys.DeriveKey(s.SecretKey)); valid(k) {
		keys = append(keys, k)
	}
	for _, fb := range s.SecretKeyFallbacks {
		if k := string(aikeys.DeriveKey(fb)); valid(k) {
			keys = append(keys, k)
		}
	}
	return keys, primaryInvalid
}

// canEncrypt ports secrets.can_encrypt.
func (s *Settings) canEncrypt() bool {
	if s.AISecretsKey != "" {
		_, _, err := aikeys.KeyMaterial(s.AISecretsKey)
		return err == nil
	}
	return true
}

// errUnreadableAI is secrets.SecretUnreadable.
var errUnreadableAI = fmt.Errorf("aikeys: secret unreadable")

// aiDecrypt decrypts a stored secret or reports SecretUnreadable.
func (s *Settings) aiDecrypt(token []byte) (string, error) {
	keys, _ := s.aiSecretKeys()
	pt, err := aikeys.Decrypt(token, keys)
	if err != nil {
		return "", errUnreadableAI
	}
	return string(pt), nil
}

// aiEncrypt ports secrets.encrypt with the primary key.
func (s *Settings) aiEncrypt(value string) (string, error) {
	keys, _ := s.aiSecretKeys()
	if len(keys) == 0 {
		return "", errUnreadableAI
	}
	return aikeys.Encrypt(value, keys[0])
}

// ------------------------------------------------------------------ styles ---

// aiStyleKeys ports apps/ai/styles: which style_config keys each style
// accepts (sorted for the "it accepts:" message).
var aiStyleKeys = map[string][]string{
	"doory":     {"extra_headers"},
	"multipart": {"extra_fields", "extra_headers", "file_field", "model_field", "options_field", "path"},
	"json":      {"body", "extra_fields", "extra_headers", "file_field", "model_field", "options_field", "path", "response_path"},
	"mask":      {"extra_fields", "extra_headers", "file_field", "model_field", "options_field", "path"},
	"custom":    {"body", "extra_fields", "extra_headers", "file_field", "method", "model_field", "options_field", "path", "response"},
}

var aiStyleLabels = map[string]string{
	"doory":     "Doory-compatible",
	"multipart": "Multipart → image",
	"json":      "JSON → base64",
	"mask":      "Mask",
	"custom":    "Custom",
}

// aiStyleConfigError ports ApiStyle.config()'s shape checks (a JSON object,
// known keys per style). The per-key reader messages (path/header/extra_fields
// values) are a pending slice — unprobed (PLAN.md M10).
func aiStyleConfigError(style string, raw any) *string {
	if raw == nil {
		return nil
	}
	if str, ok := raw.(string); ok && str == "" {
		return nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		msg := "The style settings must be a JSON object."
		return &msg
	}
	keys := aiStyleKeys[style]
	known := map[string]bool{}
	for _, k := range keys {
		known[k] = true
	}
	var unknown []string
	for k := range obj {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		allowed := strings.Join(keys, ", ")
		if allowed == "" {
			allowed = "none"
		}
		msg := fmt.Sprintf("The %s style has no setting %s (it accepts: %s).",
			aiStyleLabels[style], jsonQuote(unknown[0]), allowed)
		return &msg
	}
	return nil
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func aiStyleValid(style string) bool {
	_, ok := aiStyleLabels[style]
	return ok
}

// ---------------------------------------------------------------- tasks -----

// aiTaskIDs ports apps/ai/tasks.TASKS (one task until upstream adds more).
var aiTaskIDs = []string{"background-removal"}

func slicesContains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// aiHeaderNameOK ports check_header_name's token + reserved checks.
func aiHeaderNameOK(name string) bool {
	tok := regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	if !tok.MatchString(name) {
		return false
	}
	reserved := map[string]bool{"host": true, "content-length": true, "content-type": true,
		"transfer-encoding": true, "connection": true, "cookie": true, "user-agent": true}
	return !reserved[strings.ToLower(name)]
}

// ------------------------------------------------------------- helpers ------

func envBoolText(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envIntRange(v string, def, low, high int) int {
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < low || n > high {
		return def
	}
	return n
}

func isLatin1(s string) bool {
	for _, r := range s {
		if r > 0xFF {
			return false
		}
	}
	return true
}

// pyTypeName ports the DRF DictField message's Python type naming.
func pyTypeName(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case bool:
		return "bool"
	case float64:
		return "float"
	case nil:
		return "NoneType"
	case []any:
		return "list"
	default:
		return "dict"
	}
}

// parseUUIDShape matches an RFC 4122-ish UUID (lowercased before matching).
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func looksLikeUUID(v string) bool { return uuidPattern.MatchString(strings.ToLower(v)) }

var _ = url.Parse
