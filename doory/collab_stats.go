// GET /api/v1/collab/stats/ — staff-only observability of the real-time
// servers (apps/collab/views.py stats, IsAdminUser).

// Ports backend/apps/collab/views.py
// Ports backend/apps/collab/urls.py
package doory

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
)

func (a *App) collabStats(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return
	}
	if !u.IsStaff {
		writeDRF(w, http.StatusForbidden, "permission_denied",
			"You do not have permission to perform this action.")
		return
	}
	cutoff := collabStaleCutoff()

	// rooms: designs by live connection count (desc, then design id).
	type roomRow struct {
		design      string
		connections int
	}
	var roomRows []roomRow
	{
		rows, err := a.DB.Query(`SELECT design_id, COUNT(*) FROM collab_roomconnection
		      WHERE heartbeat_at > ? GROUP BY design_id ORDER BY COUNT(*) DESC, design_id`, cutoff)
		if err == nil {
			for rows.Next() {
				var rr roomRow
				if err := rows.Scan(&rr.design, &rr.connections); err != nil {
					break
				}
				roomRows = append(roomRows, rr)
			}
			rows.Close()
		}
	}
	var connections int
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM collab_roomconnection WHERE heartbeat_at > ?`,
		cutoff).Scan(&connections)

	byRole := map[string]int{}
	{
		rows, err := a.DB.Query(`SELECT role, COUNT(*) FROM collab_roomconnection
		      WHERE heartbeat_at > ? GROUP BY role ORDER BY role`, cutoff)
		if err == nil {
			for rows.Next() {
				var role string
				var n int
				if err := rows.Scan(&role, &n); err != nil {
					break
				}
				byRole[role] = n
			}
			rows.Close()
		}
	}

	// instances (ordered), counters summed across them.
	type instanceRow struct {
		instance    string
		sharedLayer bool
		heartbeat   string
		counters    string
	}
	var instances []instanceRow
	{
		rows, err := a.DB.Query(`SELECT instance, shared_layer, heartbeat_at, counters
		      FROM collab_collabinstance WHERE heartbeat_at > ? ORDER BY instance`, cutoff)
		if err == nil {
			for rows.Next() {
				var ir instanceRow
				if err := rows.Scan(&ir.instance, &ir.sharedLayer, &ir.heartbeat, &ir.counters); err != nil {
					break
				}
				instances = append(instances, ir)
			}
			rows.Close()
		}
	}
	totalCounters := map[string]any{}
	for _, ir := range instances {
		var partial map[string]any
		if err := jsonUnmarshalStr(ir.counters, &partial); err != nil {
			continue
		}
		for k, v := range partial {
			// Counter semantics: totals add up across instances.
			if existing, ok := totalCounters[k]; ok {
				if ef, ok1 := toFloat(existing); ok1 {
					if vf, ok2 := toFloat(v); ok2 {
						totalCounters[k] = ef + vf
						continue
					}
				}
			}
			totalCounters[k] = v
		}
	}

	// Assemble in DRF response order: rooms, connections, by_role, designs,
	// instances, counters (Go maps marshal sorted — build the body by hand).
	var b []byte
	b = append(b, `{"rooms":`...)
	b = append(b, strconv.Itoa(len(roomRows))...)
	b = append(b, `,"connections":`...)
	b = append(b, strconv.Itoa(connections)...)
	b = append(b, `,"by_role":{`...)
	first := true
	for _, role := range sortedKeysStrInt(byRole) {
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, jsonStr(role)...)
		b = append(b, ':')
		b = append(b, strconv.Itoa(byRole[role])...)
	}
	b = append(b, `},"designs":[`...)
	for i, rr := range roomRows {
		if i >= 100 {
			break
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"design":`...)
		b = append(b, jsonStr(rr.design)...)
		b = append(b, `,"connections":`...)
		b = append(b, strconv.Itoa(rr.connections)...)
		b = append(b, '}')
	}
	b = append(b, `],"instances":[`...)
	for i, ir := range instances {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"instance":`...)
		b = append(b, jsonStr(ir.instance)...)
		b = append(b, `,"shared_layer":`...)
		if ir.sharedLayer {
			b = append(b, "true"...)
		} else {
			b = append(b, "false"...)
		}
		b = append(b, `,"heartbeat_at":`...)
		b = append(b, jsonStr(renderDRFDatetime(ir.heartbeat))...)
		b = append(b, `,"counters":`...)
		if ir.counters == "" {
			b = append(b, "{}"...)
		} else {
			b = append(b, ir.counters...)
		}
		b = append(b, '}')
	}
	b = append(b, `],"counters":`...)
	cJSON, _ := jsonMarshalCompactCounters(totalCounters)
	b = append(b, cJSON...)
	b = append(b, '}')

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func sortedKeysStrInt(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

func jsonUnmarshalStr(s string, into *map[string]any) error {
	if s == "" {
		return nil
	}
	return json.Unmarshal([]byte(s), into)
}

func jsonMarshalCompactCounters(m map[string]any) ([]byte, error) {
	return collabJSON(m), nil
}

var _ = sql.ErrNoRows
