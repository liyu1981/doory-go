// ai catalogue (apps/ai/catalog.py): which options a user may pick for a task
// and how an option id resolves back to its provider.
package doory

import (
	"encoding/json"
	"strings"
)

// aiOption is catalog.Option (public() is what clients see).
type aiOption struct {
	id           string
	label        string
	task         string
	kind         string // "device" | "server"
	providerID   string
	providerName string
	owner        string // "device" | "operator" | "user"
	sendsOffsite bool
	remoteName   string
	dailyQuota   int
	env          *envProvider
	provider     *aiProviderRow
}

func (o aiOption) public() map[string]any {
	return map[string]any{
		"id": o.id, "label": o.label, "task": o.task, "kind": o.kind,
		"provider_name": o.providerName, "owner": o.owner, "sends_offsite": o.sendsOffsite,
	}
}

// userProvidersAllowed ports catalog.user_providers_allowed.
func (a *App) userProvidersAllowed(u *UserRow) bool {
	if !a.Settings.AIProvidersEnabled || u == nil {
		return false
	}
	mode := a.Settings.userProvidersMode()
	return mode == "everyone" || (mode == "staff" && u.IsStaff)
}

// aiDeviceAvailable ports catalog.device_available: no model is installed in
// the Go build (device ML models pending, PLAN.md M10), so the in-browser
// option never appears - exactly as on an install without models.
func (a *App) aiDeviceAvailable(task string) bool { return false }

// aiEnvOptions ports catalog._env_options (configured models only; the
// list_models network path is pending with the fake-provider harness).
func (a *App) aiEnvOptions(task string) []aiOption {
	var out []aiOption
	providers := a.Settings.aiEnvProviders()
	for i := range providers {
		p := providers[i]
		if !slicesContains(p.tasks, task) {
			continue
		}
		models := p.models
		if len(models) == 0 {
			models = [][2]string{{"", ""}}
		}
		for _, m := range models {
			name, lab := m[0], m[1]
			label := lab
			if label == "" {
				label = name
			}
			if label == "" {
				label = p.name
			}
			cp := p
			out = append(out, aiOption{
				id: p.id + ":" + name, label: label, task: task, kind: "server",
				providerID: p.id, providerName: p.name, owner: "operator",
				sendsOffsite: p.sendsOffsite, remoteName: name, dailyQuota: p.dailyQuota,
				env: &cp,
			})
		}
	}
	return out
}

// aiDBOptions ports catalog._db_options (provider rows with models preloaded).
func aiDBOptions(providers []aiProviderRow, task string) []aiOption {
	var out []aiOption
	for i := range providers {
		p := &providers[i]
		if !jsonStringListContains(p.tasks, task) {
			continue
		}
		type pair struct{ name, label string }
		var pairs []pair
		hasEnabled := false
		for _, m := range p.models {
			if m.task == task && m.enabled {
				pairs = append(pairs, pair{m.remoteName, m.label})
				hasEnabled = true
			}
		}
		if !hasEnabled {
			pairs = []pair{{"", ""}}
		}
		owner, daily := "user", 0
		if p.isOperator() {
			owner, daily = "operator", p.dailyQuota
		}
		for _, pr := range pairs {
			label := pr.label
			if label == "" {
				label = pr.name
			}
			if label == "" {
				label = p.name
			}
			cp := *p
			out = append(out, aiOption{
				id: p.id + ":" + pr.name, label: label, task: task, kind: "server",
				providerID: p.id, providerName: p.name, owner: owner,
				sendsOffsite: p.sendsOff, remoteName: pr.name, dailyQuota: daily,
				provider: &cp,
			})
		}
	}
	return out
}

func jsonStringListContains(rawJSON, v string) bool {
	var list []string
	_ = json.Unmarshal([]byte(rawJSON), &list)
	return slicesContains(list, v)
}

// aiOperatorProviders ports catalog.operator_providers (audience rules).
func (a *App) aiOperatorProviders(u *UserRow) ([]aiProviderRow, error) {
	rows, err := a.loadAIProviders("WHERE owner_id IS NULL AND enabled = ?", true)
	if err != nil {
		return nil, err
	}
	var out []aiProviderRow
	for _, p := range rows {
		switch p.audience {
		case "everyone":
			out = append(out, p)
		case "staff":
			if u != nil && u.IsStaff {
				out = append(out, p)
			}
		case "users":
			if u != nil && a.providerAllowsUser(p.id, u.ID) {
				out = append(out, p)
			}
		}
	}
	return out, nil
}

func (a *App) providerAllowsUser(providerID, userID string) bool {
	var n int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM ai_provider_allowed_users WHERE provider_id = ? AND user_id = ?`,
		providerID, userID).Scan(&n)
	return n > 0
}

func (a *App) aiOwnProviders(u *UserRow) ([]aiProviderRow, error) {
	if u == nil {
		return nil, nil
	}
	return a.loadAIProviders("WHERE owner_id = ? AND enabled = ?", u.ID, true)
}

// aiOptions ports catalog.options: device first (never available here), env
// providers, operator providers in the audience, then the user's own.
func (a *App) aiOptions(u *UserRow, task string) ([]aiOption, error) {
	var out []aiOption
	if a.aiDeviceAvailable(task) {
		out = append(out, aiOption{id: "device", label: "On this device",
			task: task, kind: "device", owner: "device"})
	}
	if !a.Settings.AIProvidersEnabled {
		return out, nil
	}
	out = append(out, a.aiEnvOptions(task)...)
	op, err := a.aiOperatorProviders(u)
	if err != nil {
		return nil, err
	}
	out = append(out, aiDBOptions(op, task)...)
	if a.userProvidersAllowed(u) {
		own, err := a.aiOwnProviders(u)
		if err != nil {
			return nil, err
		}
		out = append(out, aiDBOptions(own, task)...)
	}
	return out, nil
}

// aiFind ports catalog.find (only the named provider is consulted).
func (a *App) aiFind(u *UserRow, task, optionID string) (aiOption, bool) {
	if !slicesContains(aiTaskIDs, task) {
		return aiOption{}, false
	}
	if optionID == "device" {
		if a.aiDeviceAvailable(task) {
			return aiOption{id: "device", label: "On this device",
				task: task, kind: "device", owner: "device"}, true
		}
		return aiOption{}, false
	}
	if !a.Settings.AIProvidersEnabled {
		return aiOption{}, false
	}
	providerID, colon, _ := strings.Cut(optionID, ":")
	if colon == "" {
		return aiOption{}, false
	}
	var candidates []aiOption
	if strings.HasPrefix(providerID, "env-") {
		for _, o := range a.aiEnvOptions(task) {
			if o.providerID == providerID {
				candidates = append(candidates, o)
			}
		}
	} else {
		if !uuidShape.MatchString(strings.ToLower(providerID)) {
			return aiOption{}, false
		}
		if op, err := a.aiOperatorProviders(u); err == nil {
			var sel []aiProviderRow
			for _, p := range op {
				if strings.EqualFold(p.id, providerID) {
					sel = append(sel, p)
				}
			}
			candidates = append(candidates, aiDBOptions(sel, task)...)
		}
		if len(candidates) == 0 && a.userProvidersAllowed(u) {
			if own, err := a.aiOwnProviders(u); err == nil {
				var sel []aiProviderRow
				for _, p := range own {
					if strings.EqualFold(p.id, providerID) {
						sel = append(sel, p)
					}
				}
				candidates = append(candidates, aiDBOptions(sel, task)...)
			}
		}
	}
	for _, o := range candidates {
		if o.id == optionID {
			return o, true
		}
	}
	return aiOption{}, false
}
