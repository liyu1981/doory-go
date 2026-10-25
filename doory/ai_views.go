// ai views (apps/ai/views.py): the model list, the contract file, the gated
// provider CRUD and the model replace.
package doory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// aiGate ports UserProvidersAllowed: 403 ai_user_providers_disabled when the
// installation forbids the user's own servers.
func (a *App) aiGate(w http.ResponseWriter, r *http.Request) (*UserRow, bool) {
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return nil, false
	}
	if !a.Settings.AIProvidersEnabled || a.Settings.userProvidersMode() == "off" ||
		(a.Settings.userProvidersMode() == "staff" && !u.IsStaff) {
		writeDRF(w, http.StatusForbidden, "ai_user_providers_disabled",
			"Adding your own AI servers is not enabled on this installation.")
		return nil, false
	}
	return u, true
}

// aiReadBody ports BoundedJSONParser for the AI views: 256 KiB -> 413, bad
// JSON -> the DRF parse envelope.
func aiReadBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 256*1024+1))
	if err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return false
	}
	if len(raw) > 256*1024 {
		writeDRF(w, http.StatusRequestEntityTooLarge, "payload_too_large",
			"The request body is too large.")
		return false
	}
	if len(raw) == 0 {
		return true
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		writeDRF(w, http.StatusBadRequest, "parse_error", "Malformed request body.")
		return false
	}
	return true
}

// HandleModelList ports ModelListView.GET: /api/v1/ai/models/.
func (a *App) HandleModelList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return
	}
	task := r.URL.Query().Get("task")
	if task != "" && !slicesContains(aiTaskIDs, task) {
		writeDRF(w, http.StatusNotFound, "unknown_task", "Unknown task.")
		return
	}
	tasks := aiTaskIDs
	if task != "" {
		tasks = []string{task}
	}
	var list []aiOption
	for _, t := range tasks {
		opts, err := a.aiOptions(u, t)
		if err != nil {
			logerr(err, "aiModelList")
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		for _, o := range opts {
			list = append(list, o)
		}
	}
	var b bytes.Buffer
	b.WriteString("[")
	for i := range list {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(orderedOptionJSON(&list[i]))
	}
	b.WriteString("]")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

// orderedOptionJSON renders OptionSerializer fields in declaration order:
// id, label, task, kind, provider_name, owner, sends_offsite (Go maps sort
// their keys; DRF keeps serializer order).
func orderedOptionJSON(o *aiOption) string {
	esc := func(s string) string {
		j, _ := json.Marshal(s)
		return string(j)
	}
	return "{\"id\":" + esc(o.id) + ",\"label\":" + esc(o.label) +
		",\"task\":" + esc(o.task) + ",\"kind\":" + esc(o.kind) +
		",\"provider_name\":" + esc(o.providerName) + ",\"owner\":" + esc(o.owner) +
		",\"sends_offsite\":" + boolJSON(o.sendsOffsite) + "}"
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// HandleContract ports ContractView.GET: public, byte-served, cached 1h.
func (a *App) HandleContract(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline; filename=\"openapi.yaml\"")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(len(contractYAML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(contractYAML)
}

// HandleProviderList ports ProviderListView (GET/POST).
func (a *App) HandleProviderList(w http.ResponseWriter, r *http.Request) {
	u, ok := a.aiGate(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := a.loadAIProviders("WHERE owner_id = ?", u.ID)
		if err != nil {
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		var b bytes.Buffer
		b.WriteString("[")
		for i := range rows {
			if i > 0 {
				b.WriteString(",")
			}
			b.Write(orderedProviderJSON(a, &rows[i]))
		}
		b.WriteString("]")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(b.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b.Bytes())
	case http.MethodPost:
		if !enforceCSRFOr403(w, r) {
			return
		}
		var count int
		_ = a.DB.QueryRow(`SELECT COUNT(*) FROM ai_provider WHERE owner_id = ?`, u.ID).Scan(&count)
		max := a.Settings.AIMaxProvidersPerUser()
		if count >= max {
			writeDRF(w, http.StatusConflict, "limit_reached",
				fmt.Sprintf("You can add at most %d servers.", max))
			return
		}
		var body aiProviderBody
		if !aiReadBody(w, r, &body) {
			return
		}
		row, fields, ok := a.validateProvider(nil, &body, u)
		if !ok {
			writeDRFValidation(w, fields)
			return
		}
		if err := a.insertProvider(row, u.ID); err != nil {
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		loaded, err := a.loadAIProviders("WHERE id = ?", row.id)
		if err != nil || len(loaded) == 0 {
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(orderedProviderJSON(a, &loaded[0]))
	default:
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			fmt.Sprintf("Method %q not allowed.", r.Method))
	}
}

// HandleProviderDetail ports ProviderDetailView (GET/PATCH/DELETE).
func (a *App) HandleProviderDetail(w http.ResponseWriter, r *http.Request) {
	u, ok := a.aiGate(w, r)
	if !ok {
		return
	}
	p := a.ownProvider404(w, r, u)
	if p == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(orderedProviderJSON(a, p))
	case http.MethodPatch:
		if !enforceCSRFOr403(w, r) {
			return
		}
		var body aiProviderBody
		if !aiReadBody(w, r, &body) {
			return
		}
		row, fields, ok := a.validateProvider(p, &body, u)
		if !ok {
			writeDRFValidation(w, fields)
			return
		}
		if err := a.updateProvider(row); err != nil {
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		loaded, err := a.loadAIProviders("WHERE id = ?", row.id)
		if err != nil || len(loaded) == 0 {
			writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(orderedProviderJSON(a, &loaded[0]))
	case http.MethodDelete:
		if !enforceCSRFOr403(w, r) {
			return
		}
		_, _ = a.DB.Exec(`DELETE FROM ai_providermodel WHERE provider_id = ?`, p.id)
		_, _ = a.DB.Exec(`DELETE FROM ai_provider WHERE id = ?`, p.id)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			fmt.Sprintf("Method %q not allowed.", r.Method))
	}
}

// ownProvider404 ports _OwnProviders.provider (owner-scoped get_object_or_404).
func (a *App) ownProvider404(w http.ResponseWriter, r *http.Request, u *UserRow) *aiProviderRow {
	pk := routeParam(r, "uuid")
	if !uuidShape.MatchString(strings.ToLower(pk)) {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil
	}
	rows, err := a.loadAIProviders("WHERE id = ? AND owner_id = ?", strings.ToLower(pk), u.ID)
	if err != nil || len(rows) == 0 {
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return nil
	}
	return &rows[0]
}

// HandleProviderModels ports ProviderModelsView.PUT (replace the models).
func (a *App) HandleProviderModels(w http.ResponseWriter, r *http.Request) {
	u, ok := a.aiGate(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			fmt.Sprintf("Method %q not allowed.", r.Method))
		return
	}
	if !enforceCSRFOr403(w, r) {
		return
	}
	p := a.ownProvider404(w, r, u)
	if p == nil {
		return
	}
	var body struct {
		Models []aiModelBody `json:"models"`
	}
	if !aiReadBody(w, r, &body) {
		return
	}
	if fields, ok := validateProviderModels(body.Models); !ok {
		writeDRFValidation(w, fields)
		return
	}
	if err := a.replaceProviderModels(p, body.Models); err != nil {
		writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
		return
	}
	loaded, err := a.loadAIProviders("WHERE id = ?", p.id)
	if err != nil || len(loaded) == 0 {
		writeDRF(w, http.StatusInternalServerError, "error", "Server error.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(orderedProviderJSON(a, &loaded[0]))
}

// HandleProviderLoadModels ports ProviderLoadModelsView (POST
// /ai/providers/{uuid}/load-models/): a Doory-compatible server's model list
// is fetched and added for the provider's tasks. The row lookup is ported;
// the remote catalogue call (catalog.list_remote_models over the provider
// transport) is pending, so the request is refused loudly instead of
// pretending nothing happened.
func (a *App) HandleProviderLoadModels(w http.ResponseWriter, r *http.Request) {
	u, ok := a.aiGate(w, r)
	if !ok {
		return
	}
	p := a.ownProvider404(w, r, u)
	if p == nil {
		return
	}
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: ai.catalog.list_remote_models (remote model catalogue pending)")
}

// HandleProviderTest ports ProviderTestView (POST /ai/providers/{uuid}/test/):
// send the built-in sample picture and report the outcome. The row lookup is
// ported; the sample call (services.test_provider) is pending with the
// provider transport, so the request is refused loudly.
func (a *App) HandleProviderTest(w http.ResponseWriter, r *http.Request) {
	u, ok := a.aiGate(w, r)
	if !ok {
		return
	}
	p := a.ownProvider404(w, r, u)
	if p == nil {
		return
	}
	writeDRF(w, http.StatusInternalServerError, "server_error",
		"route registered but not ported: ai.services.test_provider (provider test call pending)")
}
