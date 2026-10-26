// env providers (apps/ai/env.py): AI_PROVIDER_<n>_* rows parsed into frozen
// operator providers; invalid rows are dropped and reported as one message
// each (manage.py check reports the same strings upstream).
package doory

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// envProvider is apps/ai.env.EnvProvider (frozen, operator, never persisted).
type envProvider struct {
	id             string
	name           string
	baseURL        string
	style          string
	styleConfig    map[string]any
	tasks          []string
	models         [][2]string // remote name, label
	authHeader     string
	authValue      string
	sendsOffsite   bool
	timeoutSeconds int
	maxImageBytes  int64
	dailyQuota     int
}

func (p *envProvider) hasSecret() bool { return p.authValue != "" }

// aiEnvProviders ports apps/ai.env.env_providers() (load()[0]).
func (s *Settings) aiEnvProviders() []envProvider {
	providers, _ := s.aiEnvLoad()
	return providers
}

// aiEnvLoad ports apps/ai.env.load(): (the valid providers, a message per
// invalid one).
func (s *Settings) aiEnvLoad() ([]envProvider, []string) {
	var providers []envProvider
	var errors []string
	for _, row := range s.AIProviderEnvProvider {
		provider, message := s.parseEnvProvider(row)
		if message != "" {
			errors = append(errors, message)
			continue
		}
		providers = append(providers, provider)
	}
	return providers, errors
}

// parseEnvProvider ports env.parse(): one EnvProvider or the ConfigError
// message that leaves it out.
func (s *Settings) parseEnvProvider(row AIEnvProvider) (envProvider, string) {
	prefix := "AI_PROVIDER_" + row.index + "_"
	url := strings.TrimRight(strings.TrimSpace(row.URL), "/")
	if _, terr := s.aiCheckURL(url, s.aiOperatorPolicy(), false); terr != nil {
		return envProvider{}, prefix + "URL: " + terr.message
	}
	if strings.ContainsAny(url, "?#") {
		return envProvider{}, prefix + "URL may not have a query or fragment."
	}
	style := strings.TrimSpace(row.STYLE)
	if style == "" {
		style = "doory"
	}
	if !aiStyleValid(style) {
		return envProvider{}, prefix + "STYLE: unknown style " + jsonQuote(style) + "."
	}
	var styleConfig map[string]any
	if strings.TrimSpace(row.STYLE_CONFIG) != "" {
		if err := json.Unmarshal([]byte(row.STYLE_CONFIG), &styleConfig); err != nil {
			return envProvider{}, prefix + "STYLE_CONFIG is not valid JSON."
		}
	}
	if msg := aiStyleConfigError(style, styleConfig); msg != nil {
		return envProvider{}, prefix + "STYLE_CONFIG: " + *msg
	}
	tasksSource := row.TASKS
	if strings.TrimSpace(tasksSource) == "" {
		tasksSource = "background-removal"
	}
	var tasks []string
	for _, t := range strings.Split(tasksSource, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !slicesContains(aiTaskIDs, t) {
			return envProvider{}, prefix + "TASKS: unknown task " + t + "."
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return envProvider{}, prefix + "TASKS: unknown task (none)."
	}
	header := strings.TrimSpace(row.AUTH_HEADER)
	if header != "" && !aiHeaderNameOK(header) {
		return envProvider{}, prefix + "AUTH_HEADER is not a valid HTTP header name."
	}
	value := row.AUTH_VALUE
	if strings.ContainsAny(value, "\r\n") {
		return envProvider{}, prefix + "AUTH_VALUE contains a line break."
	}
	sendsOffsite, ok := envStrictBool(row.SENDS_OFFSITE)
	if !ok {
		return envProvider{}, prefix + "SENDS_OFFSITE must be true or false."
	}
	timeout, message := envStrictInt(row.TIMEOUT_SECONDS, envInt("AI_DEFAULT_TIMEOUT_SECONDS", 60),
		prefix+"TIMEOUT_SECONDS", 1, 300)
	if message != "" {
		return envProvider{}, message
	}
	maxImage, message := envStrictInt(row.MAX_IMAGE_BYTES, 0, prefix+"MAX_IMAGE_BYTES", 0, 1<<31)
	if message != "" {
		return envProvider{}, message
	}
	quota, message := envStrictInt(row.DAILY_QUOTA, 0, prefix+"DAILY_QUOTA", 0, 1000000000)
	if message != "" {
		return envProvider{}, message
	}
	label := strings.TrimSpace(row.LABEL)
	if label == "" {
		label = "Server " + row.index
	} else if len([]rune(label)) > 80 {
		label = string([]rune(label)[:80])
	}
	return envProvider{
		id: "env-" + row.index, name: label, baseURL: url, style: style,
		styleConfig: styleConfig, tasks: tasks, models: parseEnvModels(row.MODELS),
		authHeader: header, authValue: strings.TrimSpace(value),
		sendsOffsite:   sendsOffsite,
		timeoutSeconds: timeout,
		maxImageBytes:  int64(maxImage),
		dailyQuota:     quota,
	}, ""
}

// envStrictBool ports env._bool: "", 0/no/off are false, 1/yes/on true,
// anything else is an error.
func envStrictBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false, true
	case "1", "true", "yes", "on":
		return true, true
	}
	return false, false
}

// envStrictInt ports env._int (blank: the default).
func envStrictInt(value string, def int, name string, low, high int) (int, string) {
	if strings.TrimSpace(value) == "" {
		return def, ""
	}
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, name + " must be a whole number."
	}
	if number < low || number > high {
		return 0, fmt.Sprintf("%s must be between %d and %d.", name, low, high)
	}
	return number, ""
}

// parseEnvModels ports env.parse_models: "fast:Fast,best:Best" (the label
// follows the last colon), deduplicated in order.
func parseEnvModels(value string) [][2]string {
	var out [][2]string
	seen := map[[2]string]bool{}
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, label := item, ""
		if at := strings.LastIndex(item, ":"); at >= 0 {
			name, label = item[:at], item[at+1:]
		}
		pair := [2]string{envTruncateRunes(strings.TrimSpace(name), 200),
			envTruncateRunes(strings.TrimSpace(label), 80)}
		if seen[pair] {
			continue
		}
		seen[pair] = true
		out = append(out, pair)
	}
	return out
}

func envTruncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
