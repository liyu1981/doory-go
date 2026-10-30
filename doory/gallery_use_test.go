package doory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTemplateUseCreatesDesignAndCountsUse(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("template@example.com", false, false)
	env.loginAs(user)
	doc := fillTestDocument()
	raw := string(pythonStoredJSON(encodeCompactJSON(doc)))
	now := formatDjangoDateTime(time.Now())
	id := newUUID4Hex()
	if _, err := env.db.Exec(`INSERT INTO gallery_template
		(id, created_at, updated_at, slug, title, description, tags, width, height, preset,
		 document, page_count, thumbnail, is_featured, is_published, use_count, category_id)
		VALUES (?, ?, ?, 'badge', 'Badge', '', '[]', 800, 600, '', ?, 1, '', FALSE, TRUE, 0, NULL)`,
		id, now, now, raw); err != nil {
		t.Fatal(err)
	}
	res, body := env.doJSON("/api/v1/templates/badge/use/", `{}`)
	if res.StatusCode != 201 {
		t.Fatalf("template use = %d %s", res.StatusCode, body)
	}
	var payload struct {
		ID       string `json:"id"`
		Title    string `json:"title"`
		Document json.RawMessage
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Title != "Badge" || !strings.Contains(string(payload.Document), "name-1") {
		t.Fatalf("created design = %s", body)
	}
	var uses int
	if err := env.db.QueryRow(`SELECT use_count FROM gallery_template WHERE id = ?`, id).Scan(&uses); err != nil || uses != 1 {
		t.Fatalf("use_count = %d (err %v)", uses, err)
	}
	res, body = env.doJSON("/api/v1/templates/nope/use/", `{}`)
	if res.StatusCode != 404 || !strings.Contains(body, "Template not found.") {
		t.Fatalf("missing template = %d %s", res.StatusCode, body)
	}
}
