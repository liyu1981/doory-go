// env providers (apps/ai/env.py): AI_PROVIDER_<n>_* rows parsed into frozen
// operator providers; invalid rows are dropped (manage.py check reports them
// upstream).
package doory

import (
	"encoding/json"
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

// aiEnvProviders ports apps/ai.env.load().
func (s *Settings) aiEnvProviders() []envProvider {
	var out []envProvider
	for _, row := range s.AIProviderEnvProvider {
		if p, ok := s.parseEnvProvider(row); ok {
			out = append(out, p)
		}
	}
	return out
}

func (s *Settings) parseEnvProvider(row AIEnvProvider) (envProvider, bool) {
	if row.URL == "" {
		return envProvider{}, false
	}
	if _, terr := s.aiCheckURL(row.URL, s.aiOperatorPolicy(), false); terr != nil {
		return envProvider{}, false
	}
	style := row.STYLE
	if style == "" {
		style = "doory"
	}
	if !aiStyleValid(style) {
		return envProvider{}, false
	}
	var styleConfig map[string]any
	if row.STYLE_CONFIG != "" {
		if err := json.Unmarshal([]byte(row.STYLE_CONFIG), &styleConfig); err != nil {
			return envProvider{}, false
		}
	}
	if msg := aiStyleConfigError(style, styleConfig); msg != nil {
		return envProvider{}, false
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
			return envProvider{}, false
		}
		tasks = append(tasks, t)
	}
	if len(tasks) == 0 {
		return envProvider{}, false
	}
	header := strings.TrimSpace(row.AUTH_HEADER)
	if header != "" && !aiHeaderNameOK(header) {
		return envProvider{}, false
	}
	value := row.AUTH_VALUE
	if strings.ContainsAny(value, "\r\n") {
		return envProvider{}, false
	}
	label := strings.TrimSpace(row.LABEL)
	if label == "" {
		label = "Server " + row.index
	} else if len([]rune(label)) > 80 {
		label = string([]rune(label)[:80])
	}
	return envProvider{
		id: "env-" + row.index, name: label, baseURL: row.URL, style: style,
		styleConfig: styleConfig, tasks: tasks, models: parseEnvModels(row.MODELS),
		authHeader: header, authValue: strings.TrimSpace(value),
		sendsOffsite:   envBoolText(row.SENDS_OFFSITE),
		timeoutSeconds: envIntRange(row.TIMEOUT_SECONDS, envInt("AI_DEFAULT_TIMEOUT_SECONDS", 60), 1, 300),
		maxImageBytes:  int64(envIntRange(row.MAX_IMAGE_BYTES, 0, 0, 1<<31)),
		dailyQuota:     envIntRange(row.DAILY_QUOTA, 0, 0, 1000000000),
	}, true
}

// parseEnvModels ports env.parse_models: "fast:Fast,best:Best" (the label
// follows the last colon; here the first, matching the documented shape).
func parseEnvModels(value string) [][2]string {
	var out [][2]string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, label, _ := strings.Cut(item, ":")
		out = append(out, [2]string{strings.TrimSpace(name), label})
	}
	return out
}
