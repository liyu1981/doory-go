// ai task view (apps/ai/views.py TaskView): POST /api/v1/ai/{task}/ up to the
// provider transport. The full provider exchange (send + parse + result
// asset) is pending with the fake-provider harness (PLAN.md M10).
package doory

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HandleTaskView ports TaskView.POST.
func (a *App) HandleTaskView(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil {
		writeDRF(w, http.StatusUnauthorized, "not_authenticated",
			"Authentication credentials were not provided.")
		return
	}
	task := routeParam(r, "task")
	if !slicesContains(aiTaskIDs, task) {
		writeDRF(w, http.StatusNotFound, "unknown_task", "Unknown task.")
		return
	}
	if r.Method != http.MethodPost {
		writeDRF(w, http.StatusMethodNotAllowed, "method_not_allowed",
			fmt.Sprintf("Method %q not allowed.", r.Method))
		return
	}
	if !enforceCSRFOr403(w, r) {
		return
	}
	var raw map[string]json.RawMessage
	if !aiReadBody(w, r, &raw) {
		return
	}
	// Field validation in serializer declaration order: asset_id, model, options.
	var fields []FieldError
	var assetID, model string
	if v, present := raw["asset_id"]; !present {
		fields = append(fields, SimpleField("asset_id", "This field is required.", "required"))
	} else if json.Unmarshal(v, &assetID) != nil ||
		!uuidShape.MatchString(strings.ToLower(assetID)) {
		fields = append(fields, SimpleField("asset_id", "Must be a valid UUID.", "uuid"))
	}
	if v, present := raw["model"]; !present {
		fields = append(fields, SimpleField("model", "This field is required.", "required"))
	} else if json.Unmarshal(v, &model) != nil {
		fields = append(fields, SimpleField("model", "Not a valid string.", "invalid"))
	} else if len([]rune(model)) > 300 {
		fields = append(fields, SimpleField("model",
			fmt.Sprintf("Ensure this field has no more than %d characters.", 300), "max_length"))
	}
	if v, present := raw["options"]; present {
		var anyVal any
		if err := json.Unmarshal(v, &anyVal); err == nil {
			if _, isObj := anyVal.(map[string]any); !isObj {
				fields = append(fields, SimpleField("options",
					fmt.Sprintf("Expected a dictionary of items but got type %q.", pyTypeName(anyVal)),
					"not_a_dict"))
			}
		} else {
			fields = append(fields, SimpleField("options",
				"Expected a dictionary of items but got type \"str\".", "not_a_dict"))
		}
	}
	if len(fields) > 0 {
		writeDRFValidation(w, fields)
		return
	}
	// Asset lookup first (get_object_or_404 BEFORE services.run).
	var found int
	qerr := a.DB.QueryRow(`SELECT COUNT(*) FROM assets_asset WHERE id = ? AND owner_id = ?`,
		hexUUID(assetID), u.ID).Scan(&found)
	if qerr != nil || found == 0 {
		if qerr != nil {
			logerr(qerr, "aiAssetLookup")
		}
		writeDRF(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	a.runAITask(w, u, task, model, assetID)
}

// runAITask ports services.run up to the transport: option resolution, quota,
// connection, source read, in-flight guard, then the provider dial.
func (a *App) runAITask(w http.ResponseWriter, u *UserRow, task, model, assetID string) {
	option, ok := a.aiFind(u, task, model)
	if !ok {
		writeDRF(w, http.StatusNotFound, "ai_model_not_found",
			"That option does not exist or is not available to you.")
		return
	}
	if option.kind == "device" {
		writeDRF(w, http.StatusBadRequest, "ai_runs_on_device",
			"That option runs in the browser, not on the server.")
		return
	}
	conn, code, msg := a.aiConnection(&option)
	if code != "" {
		a.logAICall(u, &option, task, model, aicallStatus(code, false), code, 0, 0, 0)
		writeDRF(w, statusForAICode(code), "ai_"+code, msg)
		return
	}
	src, err := a.readAISource(assetID, conn.maxImageBytes)
	if err != nil {
		a.logAICall(u, &option, task, model, "refused", "too_large", 0, 0, 0)
		writeDRF(w, http.StatusRequestEntityTooLarge, "ai_too_large",
			"The image is too large for this option.")
		return
	}
	// quotas.check - after connection + source (services.run order).
	if option.dailyQuota > 0 && a.aiUsedToday(u.ID, option.providerID) >= option.dailyQuota {
		writeThrottledQuota(w, a.aiSecondsUntilReset())
		return
	}
	if !a.aiEnterInFlight(u.ID) {
		writeDRF(w, http.StatusServiceUnavailable, "ai_busy",
			"Another AI task is already in progress. Try again shortly.")
		return
	}
	defer a.aiExitInFlight(u.ID)
	started := time.Now()
	body, _, berr := aiBuildDooryRequest(conn, task, option.remoteName, src)
	if berr != nil {
		a.logAICall(u, &option, task, model, "error", "bad_response", msSince(started), 0, 0)
		writeDRF(w, http.StatusBadGateway, "ai_bad_response",
			"The provider failed or answered something unusable.")
		return
	}
	policy := a.Settings.aiOperatorPolicy()
	if option.provider != nil && !option.provider.isOperator() {
		policy = a.Settings.aiUserPolicy()
	}
	if _, terr := a.Settings.aiReachable(conn.baseURL, policy); terr != nil {
		a.logAICall(u, &option, task, model, aicallStatus(terr.code, false), terr.code,
			msSince(started), len(body), 0)
		writeDRF(w, statusForAICode(terr.code), "ai_"+terr.code, terr.message)
		return
	}
	// Connected: loud pending - the provider exchange is not yet ported.
	a.logAICall(u, &option, task, model, "error", "not_supported", msSince(started), len(body), 0)
	writeDRF(w, http.StatusNotImplemented, "not_ported",
		"AI provider request pipeline not yet ported (fake-provider harness pending).")
}
