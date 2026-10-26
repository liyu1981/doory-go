// ai serializer validation (apps/ai/serializers.py): ProviderSerializer and
// ProviderModelsSerializer field rules, in DRF declaration order; object-level
// checks only when every field is clean.
package doory

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// aiProviderBody is the provider request payload (POST create / PATCH partial).
type aiProviderBody struct {
	Name          *string         `json:"name"`
	BaseURL       *string         `json:"base_url"`
	Style         *string         `json:"style"`
	StyleConfig   json.RawMessage `json:"style_config"`
	AuthHeader    *string         `json:"auth_header"`
	Secret        *string         `json:"secret"`
	Tasks         *[]string       `json:"tasks"`
	Timeout       *int            `json:"timeout_seconds"`
	MaxImageBytes *int64          `json:"max_image_bytes"`
	SendsOffsite  *bool           `json:"sends_offsite"`
	Enabled       *bool           `json:"enabled"`
	SortOrder     *int            `json:"sort_order"`
}

type aiModelBody struct {
	Task       string  `json:"task"`
	RemoteName *string `json:"remote_name"`
	Label      *string `json:"label"`
	Enabled    *bool   `json:"enabled"`
	SortOrder  *int    `json:"sort_order"`
}

// sanitizeAI strips ASCII/Unicode control characters + NULs and trims
// (SanitizedCharField).
func sanitizeAI(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == 0 || r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimFunc(b.String(), unicode.IsSpace)
}

// validateProvider ports ProviderSerializer validation.
func (a *App) validateProvider(inst *aiProviderRow, body *aiProviderBody, u *UserRow) (aiProviderRow, []FieldError, bool) {
	var row aiProviderRow
	isCreate := inst == nil
	if inst != nil {
		row = *inst
	} else {
		row = aiProviderRow{id: newUUID4HexDashed(), enabled: true, style: "doory",
			timeout: 60, styleConfig: "{}", tasks: "[]", audience: "everyone"}
		row.createdAt = formatDjangoDateTime(time.Now().UTC())
	}
	var fields []FieldError
	secretOut := []byte(nil)

	// name (SanitizedCharField, max 80; required on create)
	if body.Name != nil {
		v := sanitizeAI(*body.Name)
		if len([]rune(v)) > 80 {
			fields = append(fields, SimpleField("name",
				fmt.Sprintf("Ensure this field has no more than %d characters.", 80), "max_length"))
		} else {
			row.name = v
		}
	} else if isCreate {
		fields = append(fields, SimpleField("name", "This field is required.", "required"))
	}

	// base_url (validate_base_url: strip, user-policy form check, no
	// query/fragment, rstrip "/")
	if body.BaseURL != nil {
		v := strings.TrimSpace(*body.BaseURL)
		if _, terr := a.Settings.aiCheckURL(v, a.Settings.aiUserPolicy(), false); terr != nil {
			fields = append(fields, SimpleField("base_url", terr.message, "invalid"))
		} else if strings.ContainsAny(v, "?#") {
			fields = append(fields, SimpleField("base_url",
				"The server address may not have a query or fragment.", "invalid"))
		} else {
			row.baseURL = strings.TrimRight(v, "/")
		}
	} else if isCreate {
		fields = append(fields, SimpleField("base_url", "This field is required.", "required"))
	}

	// style (choices)
	if body.Style != nil {
		if !aiStyleValid(*body.Style) {
			fields = append(fields, SimpleField("style",
				fmt.Sprintf("%q is not a valid choice.", *body.Style), "invalid_choice"))
		} else {
			row.style = *body.Style
		}
	}

	// style_config (object + unknown keys; per-key readers pending)
	if len(body.StyleConfig) > 0 && string(body.StyleConfig) != "null" {
		var raw any
		if err := json.Unmarshal(body.StyleConfig, &raw); err != nil {
			fields = append(fields, SimpleField("style_config",
				"The style settings must be a JSON object.", "invalid"))
		} else if msg := aiStyleConfigError(row.style, raw); msg != nil {
			fields = append(fields, FieldError{Name: "style_config",
				Messages: []string{*msg}, Codes: []string{"invalid"},
				Raw: "[" + jsonStr(*msg) + "]", RawCodes: `["invalid"]`})
		} else {
			compact, _ := json.Marshal(raw)
			row.styleConfig = string(compact)
		}
	}

	// auth_header (latin-1 + token + reserved)
	if body.AuthHeader != nil {
		v := strings.TrimSpace(*body.AuthHeader)
		switch {
		case v == "":
			row.authHeader = ""
		case !isLatin1(v):
			fields = append(fields, SimpleField("auth_header",
				"The auth header must contain only Latin-1 characters.", "invalid"))
		case !aiHeaderNameOK(v):
			fields = append(fields, SimpleField("auth_header",
				"The header name is not a valid HTTP header name.", "invalid"))
		default:
			row.authHeader = v
		}
	}

	// secret (line breaks, latin-1, can_encrypt; write-only)
	secretTouched, secretValue := false, ""
	if body.Secret != nil {
		secretTouched, secretValue = true, *body.Secret
		switch {
		case strings.ContainsAny(secretValue, "\r\n"):
			fields = append(fields, SimpleField("secret",
				"The secret may not contain line breaks.", "invalid"))
		case !isLatin1(secretValue):
			fields = append(fields, SimpleField("secret",
				"The secret must contain only Latin-1 characters.", "invalid"))
		case secretValue != "" && !a.Settings.canEncrypt():
			fields = append(fields, SimpleField("secret",
				"Secrets cannot be stored: the server's secrets key is invalid.", "invalid"))
		}
	}

	// tasks (choices, deduped, non-empty)
	if body.Tasks != nil {
		var dedup []string
		seen := map[string]bool{}
		bad := -1
		for i, t := range *body.Tasks {
			if !slicesContains(aiTaskIDs, t) && bad < 0 {
				bad = i
			}
			if !seen[t] {
				seen[t] = true
				dedup = append(dedup, t)
			}
		}
		switch {
		case bad >= 0:
			msg := fmt.Sprintf("%q is not a valid choice.", (*body.Tasks)[bad])
			fields = append(fields, FieldError{Name: "tasks",
				Messages: []string{msg}, Codes: []string{"invalid_choice"},
				Raw:      fmt.Sprintf(`{"%d":[%s]}`, bad, jsonStr(msg)),
				RawCodes: fmt.Sprintf(`{"%d":["invalid_choice"]}`, bad)})
		case len(dedup) == 0:
			fields = append(fields, SimpleField("tasks", "This list may not be empty.", "invalid"))
		default:
			tj, _ := json.Marshal(dedup)
			row.tasks = string(tj)
		}
	} else if isCreate {
		fields = append(fields, SimpleField("tasks", "This list may not be empty.", "invalid"))
	}

	// timeout_seconds (Min 1 / Max 300 validators)
	if body.Timeout != nil {
		switch {
		case *body.Timeout < 1:
			fields = append(fields, SimpleField("timeout_seconds",
				"Ensure this value is greater than or equal to 1.", "min_value"))
		case *body.Timeout > 300:
			fields = append(fields, SimpleField("timeout_seconds",
				"Ensure this value is less than or equal to 300.", "max_value"))
		default:
			row.timeout = *body.Timeout
		}
	}

	// max_image_bytes (Min 0 / Max AI_MAX_IMAGE_BYTES)
	if body.MaxImageBytes != nil {
		maxV := a.Settings.AIMaxImageBytes()
		switch {
		case *body.MaxImageBytes < 0:
			fields = append(fields, SimpleField("max_image_bytes",
				"Ensure this value is greater than or equal to 0.", "min_value"))
		case *body.MaxImageBytes > maxV:
			fields = append(fields, SimpleField("max_image_bytes",
				fmt.Sprintf("Ensure this value is less than or equal to %d.", maxV), "max_value"))
		default:
			row.maxImage = *body.MaxImageBytes
		}
	}

	if body.SendsOffsite != nil {
		row.sendsOff = *body.SendsOffsite
	}
	if body.Enabled != nil {
		row.enabled = *body.Enabled
	}
	if body.SortOrder != nil {
		row.sortOrder = *body.SortOrder
	}

	if len(fields) > 0 {
		return row, fields, false
	}

	// Object-level validate(): the stored secret never follows the address to
	// another server.
	secretMsg := ""
	if inst != nil && inst.hasSecret() && !secretTouched && body.BaseURL != nil &&
		aiOrigin(inst.baseURL) != aiOrigin(row.baseURL) {
		secretMsg = "Enter the key again when changing the server address."
	} else if secretTouched {
		if secretValue == "" {
			secretOut = []byte{}
		} else if tok, err := a.Settings.aiEncrypt(secretValue); err != nil {
			secretMsg = "Secrets cannot be stored: the server's secrets key is invalid."
		} else {
			secretOut = []byte(tok)
		}
	} else if inst != nil && inst.hasSecret() && body.BaseURL != nil &&
		aiOrigin(inst.baseURL) != aiOrigin(row.baseURL) {
		// Defence in depth (serializer.save) - unreachable (validate refuses first).
		secretOut = []byte{}
	}
	if secretTouched || (inst != nil && inst.hasSecret() && body.BaseURL != nil &&
		aiOrigin(inst.baseURL) != aiOrigin(row.baseURL)) {
		row.secret = secretOut
	}
	if secretMsg != "" {
		return row, []FieldError{{Name: "secret",
			Messages: []string{secretMsg}, Codes: []string{"invalid"},
			Raw: "[" + jsonStr(secretMsg) + "]", RawCodes: `["invalid"]`}}, false
	}
	return row, nil, true
}

// aiOrigin ports models.origin (scheme, host, effective port).
func aiOrigin(rawURL string) [3]string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return [3]string{}
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := "0"
	if p := u.Port(); p != "" {
		port = p
	} else if scheme == "http" {
		port = "80"
	} else if scheme == "https" {
		port = "443"
	}
	return [3]string{scheme, host, port}
}
