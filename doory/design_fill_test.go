package doory

import (
	"encoding/json"
	"strings"
	"testing"
)

func fillTestDocument() map[string]any {
	return map[string]any{
		"schemaVersion": json.Number("1"),
		"width":         json.Number("800"),
		"height":        json.Number("600"),
		"unit":          "px",
		"pages": []any{map[string]any{
			"id":         "page-1",
			"background": map[string]any{"type": "solid", "color": "#ffffff"},
			"objects": []any{
				map[string]any{
					"id": "name-1", "type": "textbox", "text": "Name",
					"left": json.Number("10"), "top": json.Number("20"),
					"width": json.Number("300"), "height": json.Number("50"), "fontSize": json.Number("40"),
					"field": map[string]any{"key": "name", "type": "text", "label": "Name"},
				},
				map[string]any{
					"id": "accent-1", "type": "rect", "fill": "#ff0000",
					"left": json.Number("0"), "top": json.Number("0"),
					"width": json.Number("10"), "height": json.Number("10"),
					"field": map[string]any{"key": "accent", "type": "color"},
				},
			},
		}},
	}
}

func TestFillDesignCopyInPlaceAndValidation(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("fill@example.com", false, false)
	env.loginAs(user)
	doc := fillTestDocument()
	raw := pythonStoredJSON(encodeCompactJSON(doc))
	parsed, _ := parseJSONValue(raw)
	template, err := env.app.createDesign(user, "Badge", parsed, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/designs/" + dashedUUID(template.ID) + "/fill/"

	res, body := env.doJSON(path, `{"values":{"name":"Ada Lovelace","accent":"#00ff88"}}`)
	if res.StatusCode != 201 {
		t.Fatalf("copy fill = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"filled":["name","accent"]`) {
		t.Fatalf("fill body = %s", body)
	}
	var copies int
	_ = env.db.QueryRow(`SELECT COUNT(*) FROM designs_design WHERE owner_id = ?`, user.ID).Scan(&copies)
	if copies != 2 {
		t.Fatalf("design count = %d, want 2", copies)
	}
	templateNow, _ := env.app.getDesignByID(template.ID)
	if !strings.Contains(templateNow.Document, "Name") {
		t.Fatal("copy fill changed the template")
	}

	res, body = env.doJSON(path, `{"values":{"name":"Edited"},"as_copy":false}`)
	if res.StatusCode != 200 {
		t.Fatalf("in-place fill = %d %s", res.StatusCode, body)
	}
	updated, _ := env.app.getDesignByID(template.ID)
	if !strings.Contains(updated.Document, "Edited") || updated.Revision != template.Revision+1 {
		t.Fatalf("in-place document/revision = %s rev=%d", updated.Document, updated.Revision)
	}

	res, body = env.doJSON(path, `{"values":{"nope":"x","accent":"red"}}`)
	if res.StatusCode != 400 || !strings.Contains(body, `"nope"`) || !strings.Contains(body, `"accent"`) {
		t.Fatalf("bad fill = %d %s", res.StatusCode, body)
	}
}

func TestBulkFillRowsAndRowNumbers(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("bulk@example.com", false, false)
	env.loginAs(user)
	doc := fillTestDocument()
	raw := pythonStoredJSON(encodeCompactJSON(doc))
	parsed, _ := parseJSONValue(raw)
	template, err := env.app.createDesign(user, "Badge", parsed, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/designs/" + dashedUUID(template.ID) + "/bulk-fill/"
	res, body := env.doJSON(path,
		`{"rows":[{"name":"Ada"},{"accent":"nope"},{"name":"Grace"}],"row_numbers":[1,2,3],"name_pattern":"{name} ({n})"}`)
	if res.StatusCode != 200 {
		t.Fatalf("bulk fill = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, `"created":2`) || !strings.Contains(body, `"Ada (1)"`) ||
		!strings.Contains(body, `"Grace (3)"`) || !strings.Contains(body, `"ok":false`) {
		t.Fatalf("bulk body = %s", body)
	}
	var count int
	_ = env.db.QueryRow(`SELECT COUNT(*) FROM designs_design WHERE owner_id = ?`, user.ID).Scan(&count)
	if count != 3 { // template + two successful rows
		t.Fatalf("design count = %d, want 3", count)
	}
}

func TestFillDesignImportsURLImage(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("url-fill@example.com", false, false)
	env.loginAs(user)
	doc := fillTestDocument()
	page := doc["pages"].([]any)[0].(map[string]any)
	objects := page["objects"].([]any)
	page["objects"] = append(objects, map[string]any{
		"id": "photo-1", "type": "image", "src": "/media/old.png",
		"left": json.Number("0"), "top": json.Number("0"),
		"width": json.Number("100"), "height": json.Number("100"),
		"field": map[string]any{"key": "photo", "type": "image"},
	})
	raw := pythonStoredJSON(encodeCompactJSON(doc))
	parsed, _ := parseJSONValue(raw)
	template, err := env.app.createDesign(user, "Badge", parsed, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldFetch := fetchRemoteImage
	fetchRemoteImage = func(rawURL string, maxBytes int) ([]byte, error) {
		return imageBytesForRemote(t), nil
	}
	t.Cleanup(func() { fetchRemoteImage = oldFetch })
	path := "/api/v1/designs/" + dashedUUID(template.ID) + "/fill/"
	res, body := env.doJSON(path, `{"values":{"photo":"https://img.example/a.png?token=secret"}}`)
	if res.StatusCode != 201 {
		t.Fatalf("url fill = %d %s", res.StatusCode, body)
	}
	var designID string
	_ = env.db.QueryRow(`SELECT id FROM designs_design WHERE owner_id = ? AND id <> ?`,
		user.ID, template.ID).Scan(&designID)
	created, _ := env.app.getDesignByID(designID)
	parsedCreated, _ := parseJSONValue([]byte(created.Document))
	createdPage := parsedCreated.(map[string]any)["pages"].([]any)[0].(map[string]any)
	var assetID string
	for _, rawObj := range createdPage["objects"].([]any) {
		obj := rawObj.(map[string]any)
		if obj["id"] == "photo-1" {
			assetID, _ = obj["assetId"].(string)
		}
	}
	if assetID == "" {
		t.Fatalf("filled image has no assetId: %s", created.Document)
	}
	asset, err := env.app.getAssetByID(hexUUID(assetID))
	if err != nil || asset == nil {
		t.Fatalf("asset %q: %v", assetID, err)
	}
	if !strings.Contains(asset.Meta, `"source_url":"https://img.example/a.png"`) ||
		strings.Contains(asset.Meta, "secret") {
		t.Fatalf("asset meta = %s", asset.Meta)
	}
}
