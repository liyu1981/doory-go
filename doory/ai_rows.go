// ai provider rows (apps/ai/models.py): Provider/ProviderModel storage and
// the ProviderSerializer output shape (Meta.fields order).
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
)

// aiProviderRow is one apps.ai.models.Provider row.
type aiProviderRow struct {
	id          string
	createdAt   string
	updatedAt   string
	name        string
	baseURL     string
	style       string
	styleConfig string // raw JSON
	authHeader  string
	secret      []byte
	tasks       string // raw JSON list
	timeout     int
	maxImage    int64
	sendsOff    bool
	enabled     bool
	sortOrder   int
	audience    string
	dailyQuota  int
	ownerID     string // "" = operator
	models      []aiProviderModelRow
}

func (p *aiProviderRow) isOperator() bool { return p.ownerID == "" }
func (p *aiProviderRow) hasSecret() bool  { return len(p.secret) > 0 }

func (p *aiProviderRow) styleConfigMap() map[string]any {
	m := map[string]any{}
	if p.styleConfig != "" && p.styleConfig != "null" {
		_ = json.Unmarshal([]byte(p.styleConfig), &m)
	}
	return m
}

// aiProviderModelRow is apps.ai.models.ProviderModel.
type aiProviderModelRow struct {
	id         string
	createdAt  string
	updatedAt  string
	task       string
	remoteName string
	label      string
	enabled    bool
	sortOrder  int
	providerID string
}

// loadAIProviders reads provider rows with models preloaded, in Provider.Meta
// order (sort_order, name).
func (a *App) loadAIProviders(where string, args ...any) ([]aiProviderRow, error) {
	q := `SELECT id, created_at, updated_at, name, base_url, style, style_config, auth_header,
	      secret_encrypted, tasks, timeout_seconds, max_image_bytes, sends_offsite, enabled,
	      sort_order, audience, daily_quota, owner_id
	      FROM ai_provider ` + where + ` ORDER BY sort_order, name`
	rows, err := a.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiProviderRow
	for rows.Next() {
		var p aiProviderRow
		var owner sql.NullString
		var secret []byte
		if err := rows.Scan(&p.id, &p.createdAt, &p.updatedAt, &p.name, &p.baseURL, &p.style,
			&p.styleConfig, &p.authHeader, &secret, &p.tasks, &p.timeout, &p.maxImage,
			&p.sendsOff, &p.enabled, &p.sortOrder, &p.audience, &p.dailyQuota, &owner); err != nil {
			return nil, err
		}
		p.secret = secret
		if owner.Valid {
			p.ownerID = owner.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		ms, err := a.loadProviderModels(out[i].id)
		if err != nil {
			return nil, err
		}
		out[i].models = ms
	}
	return out, nil
}

func (a *App) loadProviderModels(providerID string) ([]aiProviderModelRow, error) {
	rows, err := a.DB.Query(`SELECT id, created_at, updated_at, task, remote_name, label, enabled,
	      sort_order, provider_id FROM ai_providermodel WHERE provider_id = ?
	      ORDER BY sort_order, label, remote_name`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []aiProviderModelRow
	for rows.Next() {
		var m aiProviderModelRow
		if err := rows.Scan(&m.id, &m.createdAt, &m.updatedAt, &m.task, &m.remoteName,
			&m.label, &m.enabled, &m.sortOrder, &m.providerID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// orderedProviderJSON renders ProviderSerializer output with keys in
// Meta.fields order.
func orderedProviderJSON(a *App, p *aiProviderRow) []byte {
	styleConfig := any(map[string]any{})
	if m := p.styleConfigMap(); m != nil {
		styleConfig = m
	}
	var tasks []string
	_ = json.Unmarshal([]byte(p.tasks), &tasks)
	if tasks == nil {
		tasks = []string{}
	}
	var modelsJSON strings.Builder
	modelsJSON.WriteString("[")
	for i := range p.models {
		m := &p.models[i]
		if i > 0 {
			modelsJSON.WriteString(",")
		}
		modelsJSON.WriteString(`{"id":`)
		modelsJSON.WriteString(jsonStr(m.id))
		modelsJSON.WriteString(`,"task":`)
		modelsJSON.WriteString(jsonStr(m.task))
		modelsJSON.WriteString(`,"remote_name":`)
		modelsJSON.WriteString(jsonStr(m.remoteName))
		modelsJSON.WriteString(`,"label":`)
		modelsJSON.WriteString(jsonStr(m.label))
		modelsJSON.WriteString(`,"enabled":`)
		if m.enabled {
			modelsJSON.WriteString("true")
		} else {
			modelsJSON.WriteString("false")
		}
		modelsJSON.WriteString(`,"sort_order":` + strconv.Itoa(m.sortOrder) + "}")
	}
	modelsJSON.WriteString("]")
	var b bytes.Buffer
	b.WriteString("{")
	first := true
	put := func(k string, v any) {
		if !first {
			b.WriteString(",")
		}
		first = false
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteString(":")
		vb, err := json.Marshal(v)
		if err != nil {
			vb = []byte("null")
		}
		b.Write(vb)
	}
	putRaw := func(k, raw string) {
		if !first {
			b.WriteString(",")
		}
		first = false
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteString(":")
		b.WriteString(raw)
	}
	put("id", p.id)
	put("name", p.name)
	put("base_url", p.baseURL)
	put("style", p.style)
	put("style_config", styleConfig)
	put("auth_header", p.authHeader)
	put("has_secret", p.hasSecret())
	put("tasks", tasks)
	put("timeout_seconds", p.timeout)
	put("max_image_bytes", p.maxImage)
	put("sends_offsite", p.sendsOff)
	put("enabled", p.enabled)
	put("sort_order", p.sortOrder)
	putRaw("models", modelsJSON.String())
	put("created_at", renderDRFDatetime(p.createdAt))
	put("updated_at", renderDRFDatetime(p.updatedAt))
	b.WriteString("}")
	return b.Bytes()
}
