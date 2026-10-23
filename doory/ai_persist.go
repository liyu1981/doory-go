// ai persistence: provider insert/update and the model replace
// (ProviderModelsView PUT: update_or_create per key, delete the rest).
package doory

import (
	"strconv"
	"time"
)

// insertProvider persists a created provider (serializer.save with owner).
func (a *App) insertProvider(row aiProviderRow, ownerID string) error {
	now := time.Now().UTC()
	if row.createdAt == "" {
		row.createdAt = formatDjangoDateTime(now)
	}
	row.updatedAt = formatDjangoDateTime(now)
	if row.secret == nil {
		row.secret = []byte{} // secret_encrypted is NOT NULL (Django default b"")
	}
	var owner any
	if ownerID != "" {
		owner = ownerID
	}
	_, err := a.DB.Exec(`INSERT INTO ai_provider
	      (id, created_at, updated_at, name, base_url, style, style_config, auth_header,
	       secret_encrypted, tasks, timeout_seconds, max_image_bytes, sends_offsite, enabled,
	       sort_order, audience, daily_quota, owner_id)
	      VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.id, row.createdAt, row.updatedAt, row.name, row.baseURL, row.style, row.styleConfig,
		row.authHeader, row.secret, row.tasks, row.timeout, row.maxImage, row.sendsOff,
		row.enabled, row.sortOrder, row.audience, row.dailyQuota, owner)
	return err
}

// updateProvider persists a patched provider (updated_at bumps).
func (a *App) updateProvider(row aiProviderRow) error {
	row.updatedAt = formatDjangoDateTime(time.Now().UTC())
	if row.secret == nil {
		row.secret = []byte{}
	}
	_, err := a.DB.Exec(`UPDATE ai_provider SET updated_at=?, name=?, base_url=?, style=?,
	      style_config=?, auth_header=?, secret_encrypted=?, tasks=?, timeout_seconds=?,
	      max_image_bytes=?, sends_offsite=?, enabled=?, sort_order=? WHERE id=?`,
		row.updatedAt, row.name, row.baseURL, row.style, row.styleConfig, row.authHeader,
		row.secret, row.tasks, row.timeout, row.maxImage, row.sendsOff, row.enabled,
		row.sortOrder, row.id)
	return err
}

// validateProviderModels ports ProviderModelsSerializer (max 50, unique keys,
// child task choices).
func validateProviderModels(models []aiModelBody) ([]FieldError, bool) {
	if len(models) > 50 {
		msg := "At most 50 models."
		return []FieldError{{Name: "models", Messages: []string{msg}, Codes: []string{"invalid"},
			Raw: "[" + jsonStr(msg) + "]", RawCodes: `["invalid"]`}}, false
	}
	type key struct{ task, remote string }
	seen := map[key]bool{}
	for i, m := range models {
		if !slicesContains(aiTaskIDs, m.Task) {
			msg := "Unknown task."
			return []FieldError{{Name: "models",
				Raw:      `{"` + strconv.Itoa(i) + `":{"task":[` + jsonStr(msg) + `]}}`,
				RawCodes: `{"` + strconv.Itoa(i) + `":{"task":["invalid"]}}`}}, false
		}
		remote := ""
		if m.RemoteName != nil {
			remote = sanitizeAI(*m.RemoteName)
		}
		k := key{m.Task, remote}
		if seen[k] {
			msg := "Each model may be listed once per task."
			return []FieldError{{Name: "models", Messages: []string{msg}, Codes: []string{"invalid"},
				Raw: "[" + jsonStr(msg) + "]", RawCodes: `["invalid"]`}}, false
		}
		seen[k] = true
	}
	return nil, true
}

// replaceProviderModels ports ProviderModelsView.PUT.
func (a *App) replaceProviderModels(p *aiProviderRow, models []aiModelBody) error {
	keep := map[string]bool{}
	now := formatDjangoDateTime(time.Now().UTC())
	for i, m := range models {
		remote, label := "", ""
		if m.RemoteName != nil {
			remote = sanitizeAI(*m.RemoteName)
		}
		if m.Label != nil {
			label = sanitizeAI(*m.Label)
		}
		enabled := true
		if m.Enabled != nil {
			enabled = *m.Enabled
		}
		sortOrder := i
		if m.SortOrder != nil {
			sortOrder = *m.SortOrder
		}
		var id string
		err := a.DB.QueryRow(`SELECT id FROM ai_providermodel
		      WHERE provider_id = ? AND task = ? AND remote_name = ?`,
			p.id, m.Task, remote).Scan(&id)
		if err == nil {
			_, err = a.DB.Exec(`UPDATE ai_providermodel SET updated_at=?, label=?, enabled=?, sort_order=? WHERE id=?`,
				now, label, enabled, sortOrder, id)
		} else {
			id = newUUID4HexDashed()
			_, err = a.DB.Exec(`INSERT INTO ai_providermodel
			      (id, created_at, updated_at, task, remote_name, label, enabled, sort_order, provider_id)
			      VALUES (?,?,?,?,?,?,?,?,?)`,
				id, now, now, m.Task, remote, label, enabled, sortOrder, p.id)
		}
		if err != nil {
			return err
		}
		keep[id] = true
	}
	rows, err := a.DB.Query(`SELECT id FROM ai_providermodel WHERE provider_id = ?`, p.id)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		_, _ = a.DB.Exec(`DELETE FROM ai_providermodel WHERE id = ?`, id)
	}
	_, _ = a.DB.Exec(`UPDATE ai_provider SET updated_at=? WHERE id=?`, now, p.id)
	return nil
}
