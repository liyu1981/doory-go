// Behaviour tests ported from the upstream apps/*/tests/test_admin.py files
// (G31). Each test drives the real admin stack (HTTP + form parsing + DB)
// against fixtures inserted directly, and asserts the upstream contract:
// status codes, redirect targets, stored state, and the strings the pages show.
package doory

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// exec runs one statement, failing the test on error.
func (e *adminEnv) exec(query string, args ...any) {
	e.t.Helper()
	if _, err := e.db.Exec(query, args...); err != nil {
		e.t.Fatalf("exec %.120s: %v", query, err)
	}
}

func (e *adminEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(query, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %.120s: %v", query, err)
	}
	return n
}

func (e *adminEnv) now() string { return formatDjangoDateTime(time.Now()) }

// pngUpload builds a real PNG of the requested size.
func pngUpload(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: 10, G: 120, B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png: %v", err)
	}
	return buf.Bytes()
}

func (e *adminEnv) mediaPath(rel string) string {
	return filepath.Join(e.settings.MediaRoot, filepath.FromSlash(rel))
}

// ---- designs fixtures ----

func designDocument(width, height int, objects string) string {
	return `{"schemaVersion":1,"width":` + strconv.Itoa(width) + `,"height":` + strconv.Itoa(height) +
		`,"pages":[{"id":"p1","objects":[` + objects + `]}]}`
}

func textObject(text string) string {
	encoded, _ := json.Marshal(text)
	return `{"type":"text","text":` + string(encoded) + `}`
}

func (e *adminEnv) makeDesign(owner *UserRow, title string, document string, trashed bool) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	trashedAt := any(nil)
	if trashed {
		trashedAt = now
	}
	if document == "" {
		document = designDocument(640, 480, textObject("hello"))
	}
	e.exec(`INSERT INTO designs_design
		(id, created_at, updated_at, title, width, height, preset, document, page_count, thumbnail,
		 starred, revision, trashed_at, owner_id, folder_id)
		VALUES (?, ?, ?, ?, 640, 480, '', ?, 1, '', FALSE, 1, ?, ?, NULL)`,
		id, now, now, title, document, trashedAt, owner.ID)
	return id
}

func (e *adminEnv) makeFolder(owner *UserRow, name string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	e.exec(`INSERT INTO designs_folder (id, created_at, updated_at, name, owner_id, parent_id)
		VALUES (?, ?, ?, ?, ?, NULL)`, id, now, now, name, owner.ID)
	return id
}

func (e *adminEnv) makeVersion(designID, createdBy, document string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	if document == "" {
		document = designDocument(640, 480, textObject("v-marker"))
	}
	e.exec(`INSERT INTO designs_designversion
		(id, created_at, updated_at, label, is_auto, document, page_count, thumbnail, created_by_id, design_id)
		VALUES (?, ?, ?, 'Version 1', FALSE, ?, 1, '', ?, ?)`,
		id, now, now, document, createdBy, designID)
	return id
}

// ---- accounts ----

func TestAdminAccountsSearchAndFilters(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	grace := e.makeUser("grace@example.com", false, false)
	_ = e.makeUser("ada@example.com", false, false)
	graceID := grace.ID
	e.exec(`UPDATE accounts_user SET display_name = 'Grace Hopper' WHERE id = ?`, graceID)

	body := e.mustGet("/admin/accounts/user/")
	if !strings.Contains(body, "ada@example.com") || !strings.Contains(body, "grace@example.com") {
		t.Fatalf("changelist does not list both users")
	}
	found := e.mustGet("/admin/accounts/user/?q=hopper")
	if !strings.Contains(found, "grace@example.com") || strings.Contains(found, "ada@example.com") {
		t.Fatalf("search did not narrow to Grace")
	}
	for _, query := range []string{"is_active__exact=1", "is_email_verified__exact=0", "is_staff__exact=1"} {
		e.mustGet("/admin/accounts/user/?" + query)
	}
}

func TestAdminAccountsCreateUser(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postForm("/admin/accounts/user/add/", url.Values{
		"email":           {"Fresh@Example.com"},
		"display_name":    {"Fresh"},
		"usable_password": {"true"},
		"password1":       {adminTestPassword},
		"password2":       {adminTestPassword},
	})
	e.wantStatus(res, http.StatusFound, "add user", body)
	var email, password string
	if err := e.db.QueryRow(`SELECT email, password FROM accounts_user WHERE email = 'fresh@example.com'`).
		Scan(&email, &password); err != nil {
		t.Fatalf("created user missing: %v", err)
	}
	if !verifyPassword(adminTestPassword, password) {
		t.Fatalf("created user cannot log in with the submitted password")
	}
}

func TestAdminAccountsChangePageAndEdit(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	user := e.makeUser("grace@example.com", false, false)

	// The avatar preview renders the stored media URL.
	e.exec(`UPDATE accounts_user SET avatar = 'avatars/a.webp' WHERE id = ?`, user.ID)
	page := e.mustGet("/admin/accounts/user/" + user.ID + "/change/")
	if !strings.Contains(page, mediaURL+"avatars/a.webp") {
		t.Fatalf("change page does not show the avatar URL")
	}
	if !strings.Contains(page, "Created at") || !strings.Contains(page, "Last login") {
		t.Fatalf("change page is missing the readonly date fields")
	}

	// Editing keeps the password and only changes the submitted fields.
	res, body := e.postForm("/admin/accounts/user/"+user.ID+"/change/", url.Values{
		"email":        {user.Email},
		"password":     {user.Password},
		"display_name": {"Edited"},
		"is_active":    {"on"},
	})
	e.wantStatus(res, http.StatusFound, "change user", body)
	var displayName string
	if err := e.db.QueryRow(`SELECT display_name FROM accounts_user WHERE id = ?`, user.ID).
		Scan(&displayName); err != nil || displayName != "Edited" {
		t.Fatalf("display_name = %q (err %v), want Edited", displayName, err)
	}

	// No avatar still renders.
	other := e.makeUser("plain@example.com", false, false)
	e.mustGet("/admin/accounts/user/" + other.ID + "/change/")
}

func TestAdminAccountsActions(t *testing.T) {
	e := newAdminEnv(t)
	admin := e.adminUser()
	user := e.makeUser("grace@example.com", false, false)
	postAction := func(action, id string) string {
		res, body := e.postForm("/admin/accounts/user/", url.Values{
			"action":           {action},
			"_selected_action": {id},
		})
		e.wantStatus(res, http.StatusFound, "action "+action, body)
		return res.Header.Get("Location")
	}

	postAction("mark_verified", user.ID)
	if e.count(`SELECT COUNT(*) FROM accounts_user WHERE is_email_verified = TRUE`) != 2 {
		// the acting admin was seeded verified; the target must be too
		t.Fatalf("mark_verified did not verify the selected user")
	}
	postAction("deactivate_users", user.ID)
	if e.count(`SELECT COUNT(*) FROM accounts_user WHERE id = ? AND is_active = TRUE`, user.ID) != 0 {
		t.Fatalf("deactivate_users left the user active")
	}
	postAction("activate_users", user.ID)
	if e.count(`SELECT COUNT(*) FROM accounts_user WHERE id = ? AND is_active = TRUE`, user.ID) != 1 {
		t.Fatalf("activate_users did not activate the user")
	}

	// The acting admin is never deactivated by the bulk action.
	postAction("deactivate_users", admin.ID)
	if e.count(`SELECT COUNT(*) FROM accounts_user WHERE id = ? AND is_active = TRUE`, admin.ID) != 1 {
		t.Fatalf("deactivate_users locked out the acting admin")
	}
}

func TestAdminAccountsPasswordPage(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	user := e.makeUser("grace@example.com", false, false)
	e.mustGet("/admin/accounts/user/" + user.ID + "/password/")
}

// ---- designs ----

func TestAdminDesignChangelistsAndFilters(t *testing.T) {
	e := newAdminEnv(t)
	staff := e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	e.makeDesign(owner, "Birthday card", "", false)
	e.makeDesign(owner, "Gone", "", true)

	for _, model := range []string{"design", "designversion", "folder"} {
		e.mustGet("/admin/designs/" + model + "/")
	}
	body := e.mustGet("/admin/designs/design/?trashed=yes&q=Gone")
	if !strings.Contains(body, "Gone") {
		t.Fatalf("trashed search did not return the trashed design")
	}
	body = e.mustGet("/admin/designs/design/?trashed=no")
	if !strings.Contains(body, "Birthday card") || strings.Contains(body, "Gone") {
		t.Fatalf("trashed=no did not exclude the trashed design")
	}
	_ = staff
	_ = owner
}

func TestAdminDesignDocumentReadOnlyAndEscaped(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Doc", designDocument(640, 480, textObject("visible-marker")), false)

	body := e.mustGet("/admin/designs/design/" + design + "/change/")
	if !strings.Contains(body, "visible-marker") {
		t.Fatalf("change page does not show the document")
	}
	if strings.Contains(body, `name="document"`) || strings.Contains(body, "<textarea") {
		t.Fatalf("document must not be editable")
	}

	// Inline HTML is escaped, never emitted.
	evil := e.makeDesign(owner, "Evil", designDocument(640, 480, textObject("<script>alert(1)</script>")), false)
	page := e.mustGet("/admin/designs/design/" + evil + "/change/")
	if strings.Contains(page, "<script>alert(1)</script>") || !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("document HTML was not escaped")
	}
}

func TestAdminDesignSavingKeepsTheDocument(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Before", designDocument(640, 480, textObject("keep")), false)
	res, body := e.postForm("/admin/designs/design/"+design+"/change/", url.Values{
		"title":    {"After"},
		"document": {`{"hacked": true}`},
		"revision": {"99"},
	})
	if res.StatusCode != http.StatusFound && res.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d: %.300s", res.StatusCode, body)
	}
	var title, document string
	var revision int
	if err := e.db.QueryRow(`SELECT title, document, revision FROM designs_design WHERE id = ?`, design).
		Scan(&title, &document, &revision); err != nil {
		t.Fatal(err)
	}
	if title != "After" || !strings.Contains(document, "keep") || revision != 1 {
		t.Fatalf("save changed title=%q document=%q revision=%d", title, document, revision)
	}
}

func TestAdminDesignTrashActions(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Trash me", "", false)
	action := func(name string) {
		res, body := e.postForm("/admin/designs/design/", url.Values{
			"action": {name}, "_selected_action": {design},
		})
		e.wantStatus(res, http.StatusFound, name, body)
	}
	action("move_to_trash")
	if e.count(`SELECT COUNT(*) FROM designs_design WHERE id = ? AND trashed_at IS NOT NULL`, design) != 1 {
		t.Fatalf("move_to_trash did not set trashed_at")
	}
	action("restore_from_trash")
	if e.count(`SELECT COUNT(*) FROM designs_design WHERE id = ? AND trashed_at IS NULL`, design) != 1 {
		t.Fatalf("restore_from_trash did not clear trashed_at")
	}
}

func TestAdminDesignVersionAndFolderPages(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Versioned", "", false)
	version := e.makeVersion(design, owner.ID, "")
	folder := e.makeFolder(owner, "Clients")

	body := e.mustGet("/admin/designs/designversion/" + version + "/change/")
	if !strings.Contains(body, "v-marker") {
		t.Fatalf("version change page does not show the document")
	}
	e.mustGet("/admin/designs/folder/" + folder + "/change/")
	res, page := e.get("/admin/designs/designversion/add/")
	e.wantStatus(res, http.StatusForbidden, "designversion add", page)
}

// ---- gallery ----

func TestAdminGalleryChangelistsAndCategoryOrdering(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	e.makeTemplate("One", category, 0)
	e.exec(`INSERT INTO gallery_templatecategory (id, created_at, updated_at, slug, name, icon, sort_order)
		VALUES (?, ?, ?, 'b', 'B', '', 2), (?, ?, ?, 'a', 'A', '', 1)`,
		newUUID4Hex(), e.now(), e.now(), newUUID4Hex(), e.now(), e.now())
	for _, model := range []string{"template", "templatecategory"} {
		e.mustGet("/admin/gallery/" + model + "/")
	}
	body := e.mustGet("/admin/gallery/templatecategory/")
	if strings.Index(body, ">A<") > strings.Index(body, ">B<") {
		t.Fatalf("categories are not ordered by sort_order")
	}
}

func TestAdminGalleryCreateFromJSONUpload(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Flyers", 0)
	document := `{"schemaVersion":1,"width":595,"height":842,"pages":[{"id":"p1","objects":[]},{"id":"p2","objects":[]}]}`
	fields := url.Values{
		"title": {"Autumn Flyer"}, "slug": {"autumn-flyer"}, "description": {""},
		"category": {category}, "tags": {"Autumn, Leaves"}, "preset": {""},
		"is_published": {"on"},
	}
	res, body := e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
		"document_file": {"doc.json", document},
	})
	e.wantStatus(res, http.StatusFound, "create template", body)

	var width, height, pageCount int
	var tags, createdBy string
	var categoryID string
	if err := e.db.QueryRow(`SELECT t.width, t.height, t.page_count, t.tags, t.created_by_id, t.category_id
		FROM gallery_template t WHERE t.slug = 'autumn-flyer'`).
		Scan(&width, &height, &pageCount, &tags, &createdBy, &categoryID); err != nil {
		t.Fatal(err)
	}
	if width != 595 || height != 842 || pageCount != 2 || tags != `["autumn", "leaves"]` || categoryID != category {
		t.Fatalf("stored template width=%d height=%d pages=%d tags=%s category=%s",
			width, height, pageCount, tags, categoryID)
	}
	if createdBy == "" {
		t.Fatalf("created_by was not set to the acting admin")
	}
}

func TestAdminGalleryAddRequiresAndValidatesDocument(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	fields := url.Values{
		"title": {"Autumn Flyer"}, "slug": {"autumn-flyer"}, "is_published": {"on"},
	}
	res, body := e.postMultipart("/admin/gallery/template/add/", fields, nil)
	e.wantStatus(res, http.StatusOK, "add without document", body)
	if !strings.Contains(body, "Upload the template&#39;s JSON document.") &&
		!strings.Contains(body, "Upload the template's JSON document.") {
		t.Fatalf("missing document error not shown")
	}
	if e.count(`SELECT COUNT(*) FROM gallery_template`) != 0 {
		t.Fatalf("template created without a document")
	}

	for _, payload := range []string{
		`{"schemaVersion": 1, "width": 10, "height": 10, "pages": []}`,
		`["x"]`,
		`{"pages": [{"id": "a", "objects": []}]}`,
	} {
		res, body = e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
			"document_file": {"doc.json", payload},
		})
		e.wantStatus(res, http.StatusOK, "invalid document", body)
		if e.count(`SELECT COUNT(*) FROM gallery_template`) != 0 {
			t.Fatalf("template created from invalid document %s", payload)
		}
	}
	res, body = e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
		"document_file": {"doc.json", "\xff\xfe not json"},
	})
	e.wantStatus(res, http.StatusOK, "non-json document", body)
	if e.count(`SELECT COUNT(*) FROM gallery_template`) != 0 {
		t.Fatalf("template created from a non-JSON document")
	}
}

func TestAdminGalleryThumbnailWebPAndInvalidImage(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	document := `{"schemaVersion":1,"width":595,"height":842,"pages":[{"id":"p1","objects":[]}]}`
	fields := url.Values{
		"title": {"Autumn Flyer"}, "slug": {"autumn-flyer"}, "is_published": {"on"},
	}
	res, body := e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
		"document_file": {"doc.json", document},
		"thumbnail":     {"thumb.png", string(pngUpload(t, 2000, 1000))},
	})
	e.wantStatus(res, http.StatusFound, "thumbnail upload", body)
	var thumbnail string
	if err := e.db.QueryRow(`SELECT thumbnail FROM gallery_template WHERE slug = 'autumn-flyer'`).Scan(&thumbnail); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.mediaPath(thumbnail))
	if err != nil {
		t.Fatalf("thumbnail file missing: %v", err)
	}
	width, _, format, err := decodeImageConfig(data)
	if err != nil || format != "webp" || width != 720 {
		t.Fatalf("thumbnail format=%s width=%d err=%v, want webp 720", format, width, err)
	}

	// A non-image upload is a form error, not a silently missing thumbnail.
	fields.Set("slug", "broken")
	fields.Set("title", "Broken")
	res, body = e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
		"document_file": {"doc.json", document},
		"thumbnail":     {"thumb.png", "<svg onload=alert(1)>"},
	})
	e.wantStatus(res, http.StatusOK, "invalid thumbnail", body)
	if e.count(`SELECT COUNT(*) FROM gallery_template WHERE slug = 'broken'`) != 0 {
		t.Fatalf("template created with an invalid thumbnail")
	}
}

func TestAdminGalleryThumbnailReplacementAndDeletion(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	document := `{"schemaVersion":1,"width":595,"height":842,"pages":[{"id":"p1","objects":[]}]}`
	fields := url.Values{"title": {"Autumn Flyer"}, "slug": {"autumn-flyer"}, "is_published": {"on"}}
	res, body := e.postMultipart("/admin/gallery/template/add/", fields, map[string][2]string{
		"document_file": {"doc.json", document},
		"thumbnail":     {"thumb.png", string(pngUpload(t, 900, 900))},
	})
	e.wantStatus(res, http.StatusFound, "create with thumbnail", body)
	var id, oldName string
	if err := e.db.QueryRow(`SELECT id, thumbnail FROM gallery_template`).Scan(&id, &oldName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.mediaPath(oldName)); err != nil {
		t.Fatalf("initial thumbnail missing: %v", err)
	}

	changeFields := url.Values{"title": {"Autumn Flyer"}, "slug": {"autumn-flyer"}, "is_published": {"on"}}
	res, body = e.postMultipart("/admin/gallery/template/"+id+"/change/", changeFields, map[string][2]string{
		"thumbnail": {"thumb.png", string(pngUpload(t, 500, 500))},
	})
	e.wantStatus(res, http.StatusFound, "replace thumbnail", body)
	var newName string
	if err := e.db.QueryRow(`SELECT thumbnail FROM gallery_template`).Scan(&newName); err != nil {
		t.Fatal(err)
	}
	if newName == oldName {
		t.Fatalf("thumbnail name did not change")
	}
	if _, err := os.Stat(e.mediaPath(newName)); err != nil {
		t.Fatalf("new thumbnail missing: %v", err)
	}
	if _, err := os.Stat(e.mediaPath(oldName)); !os.IsNotExist(err) {
		t.Fatalf("old thumbnail was not removed")
	}

	res, body = e.postForm("/admin/gallery/template/"+id+"/delete/", url.Values{"post": {"yes"}})
	e.wantStatus(res, http.StatusFound, "delete template", body)
	if _, err := os.Stat(e.mediaPath(newName)); !os.IsNotExist(err) {
		t.Fatalf("thumbnail file survived the delete")
	}
	if e.count(`SELECT COUNT(*) FROM gallery_template`) != 0 {
		t.Fatalf("template row survived the delete")
	}
}

func TestAdminGalleryChangingDocumentUpdatesDerivedFields(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	template := e.makeTemplate("Wide", category, 0)
	document := `{"schemaVersion":1,"width":500,"height":700,"pages":[{"id":"p1","objects":[]},
		{"id":"p2","objects":[]},{"id":"p3","objects":[]},{"id":"p4","objects":[]}]}`
	res, body := e.postMultipart("/admin/gallery/template/"+template+"/change/", url.Values{
		"title": {"Wide"}, "slug": {"wide"}, "is_published": {"on"},
	}, map[string][2]string{"document_file": {"doc.json", document}})
	e.wantStatus(res, http.StatusFound, "change document", body)
	var width, height, pages int
	if err := e.db.QueryRow(`SELECT width, height, page_count FROM gallery_template WHERE id = ?`, template).
		Scan(&width, &height, &pages); err != nil {
		t.Fatal(err)
	}
	if width != 500 || height != 700 || pages != 4 {
		t.Fatalf("derived fields = %d x %d / %d pages", width, height, pages)
	}
}

func TestAdminGalleryChangePageShowsDocumentReadOnly(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	template := e.makeTemplate("Wide", category, 0)
	// Give the template a document with a distinctive width.
	e.exec(`UPDATE gallery_template SET document = ? WHERE id = ?`,
		`{"schemaVersion":1,"width":321,"height":123,"pages":[{"id":"p1","objects":[]}]}`, template)
	body := e.mustGet("/admin/gallery/template/" + template + "/change/")
	if !strings.Contains(body, "&quot;width&quot;: 321") {
		t.Fatalf("document preview not shown: %.300s", body)
	}
	if strings.Contains(body, `name="document"`) {
		t.Fatalf("document is editable in the change form")
	}
}

func TestAdminGalleryBulkActions(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	a := e.makeTemplate("A", category, 0)
	b := e.makeTemplate("B", category, 0)
	action := func(name string, ids ...string) {
		values := url.Values{"action": {name}}
		for _, id := range ids {
			values.Add("_selected_action", id)
		}
		res, body := e.postForm("/admin/gallery/template/", values)
		e.wantStatus(res, http.StatusFound, name, body)
	}
	action("publish", a, b)
	if e.count(`SELECT COUNT(*) FROM gallery_template WHERE is_published = TRUE`) != 2 {
		t.Fatalf("publish did not publish both templates")
	}
	action("unpublish", a)
	if e.count(`SELECT COUNT(*) FROM gallery_template WHERE id = ? AND is_published = TRUE`, a) != 0 {
		t.Fatalf("unpublish left the template published")
	}
	action("feature", a)
	if e.count(`SELECT COUNT(*) FROM gallery_template WHERE id = ? AND is_featured = TRUE`, a) != 1 {
		t.Fatalf("feature did not mark the template")
	}
	action("unfeature", a)
	if e.count(`SELECT COUNT(*) FROM gallery_template WHERE id = ? AND is_featured = TRUE`, a) != 0 {
		t.Fatalf("unfeature left the template featured")
	}
}

func TestAdminGalleryDuplicateAction(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	original := e.makeTemplate("Pitch", category, 0)
	e.exec(`UPDATE gallery_template SET use_count = 9, is_featured = TRUE, tags = '["deck"]' WHERE id = ?`, original)
	action := func() {
		res, body := e.postForm("/admin/gallery/template/", url.Values{
			"action": {"duplicate"}, "_selected_action": {original},
		})
		e.wantStatus(res, http.StatusFound, "duplicate", body)
	}
	action()
	action()

	var slugs []string
	rows, err := e.db.Query(`SELECT slug FROM gallery_template WHERE id <> ? ORDER BY slug`, original)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err == nil {
			slugs = append(slugs, slug)
		}
	}
	_ = rows.Close()
	if strings.Join(slugs, ",") != "copy-of-pitch,copy-of-pitch-2" {
		t.Fatalf("duplicate slugs = %v", slugs)
	}
	var title, tags string
	var published, featured, useCount int
	if err := e.db.QueryRow(`SELECT title, tags, is_published, is_featured, use_count
		FROM gallery_template WHERE slug = 'copy-of-pitch'`).
		Scan(&title, &tags, &published, &featured, &useCount); err != nil {
		t.Fatal(err)
	}
	if title != "Copy of Pitch" || published != 0 || featured != 0 || useCount != 0 || tags != `["deck"]` {
		t.Fatalf("copy = %q published=%d featured=%d use=%d tags=%s", title, published, featured, useCount, tags)
	}
}

func TestAdminGalleryValidateActions(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	category := e.makeCategory("Cat", 0)
	good := e.makeTemplate("Good", category, 0)
	bad := e.makeTemplate("Bad", category, 0)
	e.exec(`UPDATE gallery_template SET document = '{"schemaVersion": 1}' WHERE id = ?`, bad)

	res, body := e.postForm("/admin/gallery/template/", url.Values{
		"action": {"validate_documents"}, "_selected_action": {good, bad},
	})
	e.wantStatus(res, http.StatusFound, "validate", body)
	t.Logf("set-cookie: %v", res.Header.Values("Set-Cookie"))
	// The messages ride the messages cookie; the redirected changelist shows them.
	page := e.mustGet(res.Header.Get("Location"))
	_ = page
	if !strings.Contains(page, "Good: valid") {
		t.Fatalf("valid template message missing: %.500s", page)
	}
	if !strings.Contains(page, "Bad:") || strings.Contains(page, "Bad: valid") {
		t.Fatalf("invalid template did not report an error")
	}

	// Detail actions: validate redirects back with a message, duplicate adds a row.
	res, body = e.get("/admin/gallery/template/" + good + "/validate/")
	e.wantStatus(res, http.StatusFound, "detail validate", body)
	page = e.mustGet(res.Header.Get("Location"))
	if !strings.Contains(page, "valid") {
		t.Fatalf("detail validate message missing")
	}
	res, body = e.get("/admin/gallery/template/" + good + "/duplicate/")
	e.wantStatus(res, http.StatusFound, "detail duplicate", body)
	if e.count(`SELECT COUNT(*) FROM gallery_template`) != 3 {
		t.Fatalf("detail duplicate did not create a copy")
	}
}

// ---- fixtures for gallery ----

func (e *adminEnv) makeCategory(name string, sortOrder int) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	e.exec(`INSERT INTO gallery_templatecategory (id, created_at, updated_at, slug, name, icon, sort_order)
		VALUES (?, ?, ?, ?, ?, '', ?)`, id, now, now, slug, name, sortOrder)
	return id
}

func (e *adminEnv) makeTemplate(title, category string, published int) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	slug := strings.ToLower(strings.ReplaceAll(title, " ", "-"))
	document := `{"schemaVersion":1,"width":1080,"height":1920,"pages":[{"id":"p1","objects":[]}]}`
	e.exec(`INSERT INTO gallery_template
		(id, created_at, updated_at, slug, title, description, tags, width, height, preset, document,
		 page_count, thumbnail, is_featured, is_published, use_count, created_by_id, category_id)
		VALUES (?, ?, ?, ?, ?, '', '[]', 1080, 1920, '', ?, 1, '', FALSE, ?, 0, NULL, ?)`,
		id, now, now, slug, title, document, published, nullIfEmpty(category))
	return id
}

// ---- elements ----

const adminRedSquare = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 40 20" width="40" height="20"><rect width="40" height="20" fill="#e11d48"/></svg>`
const adminCurrentColor = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><path d="M0 0h24v24H0z" fill="currentColor"/></svg>`
const adminEvilSVG = `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 10 10" onload="alert(1)"><script>alert(1)</script><foreignObject><div>x</div></foreignObject><a xlink:href="javascript:alert(1)"><circle r="1"/></a><rect width="10" height="10" fill="#ff0000" onclick="x()"/><image href="https://evil.example/x.png" width="1" height="1"/></svg>`

func (e *adminEnv) makePack(slug, kind string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	e.exec(`INSERT INTO elements_elementpack
		(id, created_at, updated_at, slug, name, kind, license, source_url, description, thumbnail,
		 sort_order, is_published, attribution, license_url, source_ref, sync_fingerprint, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, 'CC0-1.0', '', '', '', 0, TRUE, '', '', '', '', NULL)`,
		id, now, now, slug, packTitle(slug), kind)
	return id
}

func (e *adminEnv) makeElement(packID, name, svg string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	key := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	e.exec(`INSERT INTO elements_element
		(id, created_at, updated_at, key, name, kind, style, tags, search_name, search_tags,
		 content_hash, svg, width, height, colors, is_recolorable, is_published, sort_order, pack_id)
		VALUES (?, ?, ?, ?, ?, 'shape', '', '[]', ?, '', '', ?, 40, 20, '["#e11d48"]', FALSE, TRUE, 0, ?)`,
		id, now, now, key, name, strings.ToLower(name), svg, packID)
	return id
}

func packTitle(slug string) string {
	words := strings.Split(strings.ReplaceAll(slug, "-", " "), " ")
	for i, word := range words {
		if word != "" {
			words[i] = strings.ToUpper(word[:1]) + word[1:]
		}
	}
	return strings.Join(words, " ")
}

func TestAdminElementsChangelistsAndForms(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	element := e.makeElement(pack, "Square", "elements/shapes/square.svg")
	for _, model := range []string{"elementpack", "element"} {
		e.mustGet("/admin/elements/" + model + "/")
	}
	e.mustGet("/admin/elements/elementpack/add/")
	e.mustGet("/admin/elements/element/add/")
	e.mustGet("/admin/elements/element/" + element + "/change/")
	body := e.mustGet("/admin/elements/elementpack/" + pack + "/change/")
	if !strings.Contains(body, mediaURL+"elements/shapes/square.svg") {
		t.Fatalf("pack page does not list the element SVG")
	}
}

func TestAdminElementAddSanitisesSVG(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	res, body := e.postMultipart("/admin/elements/element/add/", url.Values{
		"pack": {pack}, "name": {"Uploaded"}, "kind": {"shape"},
		"tags": {"Alpha, beta, ALPHA"}, "is_published": {"on"}, "sort_order": {"0"},
	}, map[string][2]string{"svg": {"shape.svg", adminEvilSVG}})
	e.wantStatus(res, http.StatusFound, "add element", body)

	var svg, tags, colors string
	if err := e.db.QueryRow(`SELECT svg, tags, colors FROM elements_element`).
		Scan(&svg, &tags, &colors); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(e.mediaPath(svg))
	if err != nil {
		t.Fatalf("stored SVG missing: %v", err)
	}
	text := string(stored)
	if strings.Contains(text, "script") || strings.Contains(text, "onclick") || strings.Contains(text, "evil.example") {
		t.Fatalf("SVG was not sanitised: %s", text)
	}
	if tags != `["alpha", "beta"]` {
		t.Fatalf("tags = %s, want [alpha beta]", tags)
	}
	if !strings.Contains(colors, "#ff0000") {
		t.Fatalf("colors = %s", colors)
	}
}

func TestAdminElementAddRejectsNonSVG(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	for _, payload := range []string{"not xml", "<html/>", ""} {
		res, body := e.postMultipart("/admin/elements/element/add/", url.Values{
			"pack": {pack}, "name": {"Bad"}, "kind": {"shape"},
		}, map[string][2]string{"svg": {"shape.svg", payload}})
		e.wantStatus(res, http.StatusOK, "non-svg upload", body)
		if e.count(`SELECT COUNT(*) FROM elements_element`) != 0 {
			t.Fatalf("element created from %q", payload)
		}
	}
}

func TestAdminElementReplaceSVGRefreshesMetadata(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	element := e.makeElement(pack, "Square", "elements/shapes/square.svg")
	res, body := e.postMultipart("/admin/elements/element/"+element+"/change/", url.Values{
		"pack": {pack}, "name": {"Square"}, "kind": {"shape"}, "is_published": {"on"},
	}, map[string][2]string{"svg": {"icon.svg", adminCurrentColor}})
	e.wantStatus(res, http.StatusFound, "replace svg", body)
	var recolorable, width int
	if err := e.db.QueryRow(`SELECT is_recolorable, width FROM elements_element WHERE id = ?`, element).
		Scan(&recolorable, &width); err != nil {
		t.Fatal(err)
	}
	if recolorable != 0 || width != 24 {
		t.Fatalf("metadata recolorable=%d width=%d, want 0/24", recolorable, width)
	}
}

func TestAdminBulkUploadPermissions(t *testing.T) {
	e := newAdminEnv(t)
	pack := e.makePack("icons", "icon")
	url := "/admin/elements/elementpack/" + pack + "/upload/"
	res, body := e.get(url)
	e.wantStatus(res, http.StatusFound, "anonymous upload", body)

	editor := e.makeUser("editor@example.com", true, false)
	e.loginAs(editor)
	res, body = e.get(url)
	e.wantStatus(res, http.StatusForbidden, "upload without perms", body)

	e.grant(editor, "elements.change_elementpack")
	res, body = e.get(url)
	e.wantStatus(res, http.StatusForbidden, "upload without add_element", body)

	e.grant(editor, "elements.add_element")
	e.mustGet(url)
}

func TestAdminBulkUploadManyFiles(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("icons", "icon")
	res, body := e.postFiles("/admin/elements/elementpack/"+pack+"/upload/", url.Values{"kind": {""}},
		[]adminFilePart{
			{"files", "Red Box.svg", adminRedSquare},
			{"files", "mono-icon.svg", adminCurrentColor},
			{"files", "evil.svg", adminEvilSVG},
			{"files", "broken.svg", "nope"},
		})
	e.wantStatus(res, http.StatusFound, "bulk upload", body)

	var names []string
	rows, err := e.db.Query(`SELECT name FROM elements_element ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	_ = rows.Close()
	if strings.Join(names, ",") != "Evil,Mono Icon,Red Box" {
		t.Fatalf("uploaded names = %v", names)
	}
	if e.count(`SELECT COUNT(*) FROM elements_element WHERE kind = 'icon'`) != 3 {
		t.Fatalf("bulk upload did not apply the pack kind")
	}
	page := e.mustGet(res.Header.Get("Location"))
	if !strings.Contains(page, "broken.svg:") {
		t.Fatalf("broken file message missing: %.400s", page)
	}
	if !strings.Contains(page, "3 added") {
		t.Fatalf("summary message missing")
	}
	rows, _ = e.db.Query(`SELECT svg FROM elements_element`)
	for rows.Next() {
		var svg string
		if err := rows.Scan(&svg); err != nil {
			continue
		}
		data, rerr := os.ReadFile(e.mediaPath(svg))
		if rerr != nil || bytes.Contains(data, []byte("script")) {
			t.Fatalf("stored SVG is missing or unsanitised: %v", rerr)
		}
	}
	_ = rows.Close()
}

func TestAdminBulkUploadReplacesArtworkAndAppliesKind(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	uploadURL := "/admin/elements/elementpack/" + pack + "/upload/"
	if res, body := e.postFiles(uploadURL, url.Values{"kind": {""}},
		[]adminFilePart{{"files", "box.svg", adminRedSquare}}); res.StatusCode != http.StatusFound {
		t.Fatalf("first upload = %d: %.300s", res.StatusCode, body)
	}
	if res, body := e.postFiles(uploadURL, url.Values{"kind": {""}},
		[]adminFilePart{{"files", "box.svg", adminCurrentColor}}); res.StatusCode != http.StatusFound {
		t.Fatalf("second upload = %d: %.300s", res.StatusCode, body)
	}
	if e.count(`SELECT COUNT(*) FROM elements_element`) != 1 {
		t.Fatalf("re-upload created a second element")
	}
	if e.count(`SELECT COUNT(*) FROM elements_element WHERE is_recolorable = FALSE`) != 1 {
		t.Fatalf("re-upload did not refresh is_recolorable")
	}
	if res, body := e.postFiles(uploadURL, url.Values{"kind": {"sticker"}},
		[]adminFilePart{{"files", "a.svg", adminRedSquare}}); res.StatusCode != http.StatusFound {
		t.Fatalf("kind upload = %d: %.300s", res.StatusCode, body)
	}
	if e.count(`SELECT COUNT(*) FROM elements_element WHERE kind = 'sticker'`) != 1 {
		t.Fatalf("chosen kind was not applied")
	}
	// An empty submit is a valid form; it just adds nothing.
	if res, body := e.postForm(uploadURL, url.Values{"kind": {""}}); res.StatusCode != http.StatusOK {
		t.Fatalf("empty submit = %d: %.300s", res.StatusCode, body)
	}
}

func TestAdminBulkUploadUnknownPackIs404(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.get("/admin/elements/elementpack/00000000000000000000000000000000/upload/")
	e.wantStatus(res, http.StatusNotFound, "unknown pack upload", body)
}

func TestAdminPackThumbnailIsWebP(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postMultipart("/admin/elements/elementpack/add/", url.Values{
		"name": {"Pack"}, "slug": {"pack"}, "kind": {"shape"}, "license": {"CC0-1.0"},
		"source_url": {""}, "description": {""}, "sort_order": {"0"}, "is_published": {"on"},
	}, map[string][2]string{"thumbnail": {"t.png", string(pngUpload(t, 900, 600))}})
	e.wantStatus(res, http.StatusFound, "pack thumbnail", body)
	var thumbnail string
	if err := e.db.QueryRow(`SELECT thumbnail FROM elements_elementpack`).Scan(&thumbnail); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.mediaPath(thumbnail))
	if err != nil {
		t.Fatalf("pack thumbnail missing: %v", err)
	}
	width, _, format, err := decodeImageConfig(data)
	if err != nil || format != "webp" || width != 480 {
		t.Fatalf("pack thumbnail format=%s width=%d err=%v, want webp 480", format, width, err)
	}
}

func TestAdminElementPublishActions(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	first := e.makeElement(pack, "A", "elements/shapes/a.svg")
	second := e.makeElement(pack, "B", "elements/shapes/b.svg")
	action := func(path, name string, ids ...string) {
		values := url.Values{"action": {name}}
		for _, id := range ids {
			values.Add("_selected_action", id)
		}
		res, body := e.postForm(path, values)
		e.wantStatus(res, http.StatusFound, name, body)
	}
	action("/admin/elements/element/", "unpublish", first, second)
	if e.count(`SELECT COUNT(*) FROM elements_element WHERE is_published = TRUE`) != 0 {
		t.Fatalf("unpublish left elements published")
	}
	action("/admin/elements/element/", "publish", first)
	if e.count(`SELECT COUNT(*) FROM elements_element WHERE is_published = TRUE`) != 1 {
		t.Fatalf("publish did not publish one element")
	}
	action("/admin/elements/elementpack/", "unpublish", pack)
	if e.count(`SELECT COUNT(*) FROM elements_elementpack WHERE is_published = TRUE`) != 0 {
		t.Fatalf("pack unpublish did not take effect")
	}
}

func TestAdminElementUploadActionRedirects(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	res, body := e.get("/admin/elements/elementpack/" + pack + "/upload-svgs/")
	e.wantStatus(res, http.StatusFound, "upload action", body)
	if want := "/admin/elements/elementpack/" + dashedUUID(pack) + "/upload/"; res.Header.Get("Location") != want {
		t.Fatalf("Location = %q, want %q", res.Header.Get("Location"), want)
	}
}

func TestAdminElementStyleAndSearchColumns(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	element := e.makeElement(pack, "Old", "elements/shapes/old.svg")
	res, body := e.postForm("/admin/elements/element/"+element+"/change/", url.Values{
		"pack": {pack}, "name": {"Café Au Lait"}, "kind": {"shape"}, "style": {"filled"},
		"tags": {"Drink, Hot"}, "is_published": {"on"}, "svg": {""},
	})
	e.wantStatus(res, http.StatusFound, "change element", body)
	var style, searchName, searchTags string
	if err := e.db.QueryRow(`SELECT style, search_name, search_tags FROM elements_element WHERE id = ?`, element).
		Scan(&style, &searchName, &searchTags); err != nil {
		t.Fatal(err)
	}
	if style != "filled" || searchName != "cafe au lait" || searchTags != " drink hot " {
		t.Fatalf("style=%q search_name=%q search_tags=%q", style, searchName, searchTags)
	}
}

func TestAdminElementRejectsUnknownStyle(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("shapes", "shape")
	res, body := e.postMultipart("/admin/elements/element/add/", url.Values{
		"pack": {pack}, "name": {"Bad"}, "kind": {"shape"}, "style": {"neon"},
	}, map[string][2]string{"svg": {"shape.svg", adminRedSquare}})
	e.wantStatus(res, http.StatusOK, "unknown style", body)
	if e.count(`SELECT COUNT(*) FROM elements_element`) != 0 {
		t.Fatalf("element created with an unknown style")
	}
}

func TestAdminPackLicenceAndSourceRef(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	pack := e.makePack("demo-icons", "icon")
	e.exec(`UPDATE elements_elementpack SET license = 'MIT', source_ref = 'demo-icons@1.0.0' WHERE id = ?`, pack)
	page := e.mustGet("/admin/elements/elementpack/" + pack + "/change/")
	if !strings.Contains(page, "demo-icons@1.0.0") || !strings.Contains(page, "attribution") {
		t.Fatalf("synced release or attribution missing from the pack page")
	}
	res, body := e.postForm("/admin/elements/elementpack/"+pack+"/change/", url.Values{
		"name": {"Demo Icons"}, "slug": {"demo-icons"}, "kind": {"icon"}, "license": {"MIT"},
		"license_url": {"https://example.com/LICENSE"},
		"attribution": {"Demo: Copyright (c) 2026 Example (MIT)."},
		"source_url":  {""}, "description": {""}, "sort_order": {"0"}, "is_published": {"on"},
	})
	e.wantStatus(res, http.StatusFound, "pack change", body)
	var licenseURL, attribution, sourceRef string
	if err := e.db.QueryRow(`SELECT license_url, attribution, source_ref FROM elements_elementpack WHERE id = ?`, pack).
		Scan(&licenseURL, &attribution, &sourceRef); err != nil {
		t.Fatal(err)
	}
	if licenseURL != "https://example.com/LICENSE" ||
		attribution != "Demo: Copyright (c) 2026 Example (MIT)." || sourceRef != "demo-icons@1.0.0" {
		t.Fatalf("licence_url=%q attribution=%q source_ref=%q", licenseURL, attribution, sourceRef)
	}
}

// ---- fonts ----

func (e *adminEnv) makeFontFamily(name, ownerID string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	slug := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	var owner any
	if ownerID != "" {
		owner = ownerID
	}
	e.exec(`INSERT INTO fonts_fontfamily
		(id, created_at, updated_at, family, slug, category, scripts, is_variable, popularity,
		 license, source, is_published, owner_id)
		VALUES (?, ?, ?, ?, ?, 'sans-serif', '[]', FALSE, 0, 'OFL-1.1', 'fontsource', TRUE, ?)`,
		id, now, now, name, slug, owner)
	variant := newUUID4Hex()
	storage := "fonts/system/" + slug + "/400-normal-latin.woff2"
	e.exec(`INSERT INTO fonts_fontvariant
		(id, created_at, updated_at, weight, style, subset, unicode_range, file, format, size, family_id)
		VALUES (?, ?, ?, 400, 'normal', 'latin', '', ?, 'woff2', 204, ?)`,
		variant, now, now, storage, id)
	if err := adminWriteMedia(e.settings.MediaRoot, storage, append([]byte("wOF2"), make([]byte, 200)...)); err != nil {
		e.t.Fatalf("write variant: %v", err)
	}
	return id
}

func fontForm(normal bool) url.Values {
	values := url.Values{
		"family": {"Staff Pick"}, "slug": {"staff-pick"}, "category": {"serif"},
		"scripts": {"[]"}, "popularity": {"5"}, "source": {"fontsource"}, "license": {"OFL-1.1"},
		"variants-TOTAL_FORMS": {"1"}, "variants-INITIAL_FORMS": {"0"},
		"variants-MIN_NUM_FORMS": {"0"}, "variants-MAX_NUM_FORMS": {"1000"},
		"variants-0-weight": {"400"}, "variants-0-style": {"normal"}, "variants-0-subset": {""},
	}
	if normal {
		values.Set("is_published", "on")
	}
	return values
}

func readFontFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "test", "parity", "testdata", "fonts", "fixture.ttf"))
	if err != nil {
		t.Fatalf("font fixture missing: %v", err)
	}
	return string(data)
}

func TestAdminFontChangelistPreviewsAndForms(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	family := e.makeFontFamily("Open Sans", "")
	body := e.mustGet("/admin/fonts/fontfamily/")
	if !strings.Contains(body, "doory-preview-"+family) || !strings.Contains(body, "The quick brown fox") {
		t.Fatalf("changelist preview missing: %.400s", body)
	}
	e.mustGet("/admin/fonts/fontfamily/add/")
	e.mustGet("/admin/fonts/fontfamily/" + family + "/change/")
}

func TestAdminFontAddWithValidatedFile(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postFiles("/admin/fonts/fontfamily/add/", fontForm(true), []adminFilePart{
		{"variants-0-file", "brand.ttf", readFontFixture(t)},
	})
	e.wantStatus(res, http.StatusFound, "add family", body)

	var familyID string
	var owner sql.NullString
	if err := e.db.QueryRow(`SELECT id, owner_id FROM fonts_fontfamily WHERE slug = 'staff-pick'`).
		Scan(&familyID, &owner); err != nil {
		t.Fatalf("family missing: %v", err)
	}
	if owner.Valid {
		t.Fatalf("owner_id = %q, want NULL for an admin-created family", owner.String)
	}
	var format, file string
	var size int
	if err := e.db.QueryRow(`SELECT format, size, file FROM fonts_fontvariant WHERE family_id = ?`, familyID).
		Scan(&format, &size, &file); err != nil {
		t.Fatalf("variant missing: %v", err)
	}
	if format != "ttf" || size <= 0 || !strings.HasPrefix(file, "fonts/system/staff-pick/") ||
		!strings.HasSuffix(file, ".ttf") {
		t.Fatalf("variant format=%q size=%d file=%q", format, size, file)
	}
	if _, err := os.Stat(e.mediaPath(file)); err != nil {
		t.Fatalf("variant file missing: %v", err)
	}
}

func TestAdminFontRejectsFilesThatAreNotFonts(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postFiles("/admin/fonts/fontfamily/add/", fontForm(true), []adminFilePart{
		{"variants-0-file", "brand.ttf", "this is not a font at all"},
	})
	e.wantStatus(res, http.StatusOK, "non-font upload", body)
	if e.count(`SELECT COUNT(*) FROM fonts_fontfamily`) != 0 ||
		e.count(`SELECT COUNT(*) FROM fonts_fontvariant`) != 0 {
		t.Fatalf("a family/variant was created from a non-font upload")
	}
}

func TestAdminFontPublishActions(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	a := e.makeFontFamily("A", "")
	b := e.makeFontFamily("B", "")
	action := func(name string, ids ...string) {
		values := url.Values{"action": {name}}
		for _, id := range ids {
			values.Add("_selected_action", id)
		}
		res, body := e.postForm("/admin/fonts/fontfamily/", values)
		e.wantStatus(res, http.StatusFound, name, body)
	}
	action("unpublish", a, b)
	if e.count(`SELECT COUNT(*) FROM fonts_fontfamily WHERE is_published = TRUE`) != 0 {
		t.Fatalf("unpublish left families published")
	}
	action("publish", a)
	if e.count(`SELECT COUNT(*) FROM fonts_fontfamily WHERE is_published = TRUE`) != 1 {
		t.Fatalf("publish did not publish one family")
	}
}

func TestAdminFontOwnerCannotBeReassigned(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	family := e.makeFontFamily("Mine", owner.ID)
	body := e.mustGet("/admin/fonts/fontfamily/" + family + "/change/")
	if strings.Contains(body, `name="owner"`) {
		t.Fatalf("owner is editable in the font form")
	}
	if !strings.Contains(body, "owner@example.com") {
		t.Fatalf("owner email missing from the font page")
	}
}

// ---- sharing ----

func (e *adminEnv) makeShareLink(designID, createdBy, role, token, passwordHash string, active bool) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	e.exec(`INSERT INTO sharing_sharelink
		(id, created_at, updated_at, token, role, is_active, expires_at, password_hash,
		 last_accessed_at, access_count, created_by_id, design_id)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, NULL, 0, ?, ?)`,
		id, now, now, token, role, boolSQL(active), passwordHash, nullIfEmpty(createdBy), designID)
	return id
}

func (e *adminEnv) makeComment(designID, authorID, body string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	e.exec(`INSERT INTO sharing_comment
		(id, created_at, updated_at, body, page_id, x, y, resolved, author_id, design_id, parent_id)
		VALUES (?, ?, ?, ?, '', NULL, NULL, FALSE, ?, ?, NULL)`,
		id, now, now, body, authorID, designID)
	return id
}

func TestAdminShareLinkHidesSecrets(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Summer poster", "", false)
	link := e.makeShareLink(design, owner.ID, "edit", "tok-visible-marker-123", "pbkdf2_sha256$1500000$abcdefghijkl$markerhashvalue", true)

	if res, body := e.get("/admin/sharing/sharelink/"); res.StatusCode != http.StatusOK {
		t.Fatalf("changelist = %d: %.300s", res.StatusCode, body)
	}
	body := e.mustGet("/admin/sharing/sharelink/" + link + "/change/")
	if strings.Contains(body, "tok-visible-marker-123") {
		t.Fatalf("the share token leaked into the change page")
	}
	if strings.Contains(body, "markerhashvalue") {
		t.Fatalf("the password hash leaked into the change page")
	}
}

func TestAdminShareLinksCannotBeCreated(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.get("/admin/sharing/sharelink/add/")
	e.wantStatus(res, http.StatusForbidden, "sharelink add", body)
}

func TestAdminStaffCanSwitchAShareLinkOff(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Summer poster", "", false)
	link := e.makeShareLink(design, owner.ID, "view", "tok-off-1", "", true)
	res, body := e.postForm("/admin/sharing/sharelink/"+link+"/change/", url.Values{})
	e.wantStatus(res, http.StatusFound, "switch link off", body)
	if e.count(`SELECT COUNT(*) FROM sharing_sharelink WHERE id = ? AND is_active = TRUE`, link) != 0 {
		t.Fatalf("share link is still active")
	}
}

func TestAdminCommentIsReadOnlyButDeletable(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	design := e.makeDesign(owner, "Summer poster", "", false)
	comment := e.makeComment(design, owner.ID, "moderate me")
	e.mustGet("/admin/sharing/comment/")
	res, body := e.get("/admin/sharing/comment/add/")
	e.wantStatus(res, http.StatusForbidden, "comment add", body)
	res, body = e.postForm("/admin/sharing/comment/"+comment+"/delete/", url.Values{"post": {"yes"}})
	e.wantStatus(res, http.StatusFound, "comment delete", body)
	if e.count(`SELECT COUNT(*) FROM sharing_comment`) != 0 {
		t.Fatalf("comment survived the delete")
	}
}

// ---- ai ----

const adminAISecret = "Bearer admin-secret-value-321"

func aiProviderForm(baseURL string, extra map[string]string) url.Values {
	values := url.Values{
		"name": {"Server X"}, "base_url": {baseURL}, "style": {"multipart"},
		"style_config": {`{"path": "/api/remove"}`}, "timeout_seconds": {"60"},
		"max_image_bytes": {"0"}, "auth_header": {"Authorization"}, "secret": {""},
		"tasks": {"background-removal"}, "enabled": {"on"}, "sort_order": {"0"},
		"audience": {"everyone"}, "daily_quota": {"0"},
		"models-TOTAL_FORMS": {"0"}, "models-INITIAL_FORMS": {"0"},
		"models-MIN_NUM_FORMS": {"0"}, "models-MAX_NUM_FORMS": {"1000"},
	}
	for key, value := range extra {
		if value == "" {
			values.Del(key)
			continue
		}
		values.Set(key, value)
	}
	return values
}

func TestAdminProviderStoresTheSecretEncrypted(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postForm("/admin/ai/provider/add/", aiProviderForm("http://127.0.0.1:7001",
		map[string]string{"secret": adminAISecret}))
	e.wantStatus(res, http.StatusFound, "add provider", body)

	var id string
	var secret []byte
	var owner sql.NullString
	if err := e.db.QueryRow(`SELECT id, secret_encrypted, owner_id FROM ai_provider`).
		Scan(&id, &secret, &owner); err != nil {
		t.Fatalf("provider missing: %v", err)
	}
	if owner.Valid {
		t.Fatalf("operator provider owner_id = %q, want NULL", owner.String)
	}
	plain, err := e.settings.aiDecrypt(secret)
	if err != nil || plain != adminAISecret {
		t.Fatalf("stored secret = %q (err %v), want %q", plain, err, adminAISecret)
	}
	page := e.mustGet("/admin/ai/provider/" + id + "/change/")
	if strings.Contains(page, "admin-secret-value") {
		t.Fatalf("the secret reached the change page")
	}
	if !strings.Contains(page, "Set (hidden)") {
		t.Fatalf("the change page does not report the stored secret")
	}
}

func TestAdminProviderMovingHostDropsTheSecret(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postForm("/admin/ai/provider/add/", aiProviderForm("http://127.0.0.1:7001",
		map[string]string{"secret": adminAISecret}))
	e.wantStatus(res, http.StatusFound, "add provider", body)
	var id string
	if err := e.db.QueryRow(`SELECT id FROM ai_provider`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	res, body = e.postForm("/admin/ai/provider/"+id+"/change/", aiProviderForm("http://other.example:7000", nil))
	e.wantStatus(res, http.StatusFound, "move provider", body)
	var secret []byte
	if err := e.db.QueryRow(`SELECT secret_encrypted FROM ai_provider WHERE id = ?`, id).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if len(secret) != 0 {
		t.Fatalf("the secret survived a host move")
	}
}

func TestAdminProviderSecretKeepAndClear(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postForm("/admin/ai/provider/add/", aiProviderForm("http://127.0.0.1:7001",
		map[string]string{"secret": adminAISecret}))
	e.wantStatus(res, http.StatusFound, "add provider", body)
	var id string
	if err := e.db.QueryRow(`SELECT id FROM ai_provider`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	res, body = e.postForm("/admin/ai/provider/"+id+"/change/", aiProviderForm("http://127.0.0.1:7001", nil))
	e.wantStatus(res, http.StatusFound, "keep secret", body)
	var secret []byte
	if err := e.db.QueryRow(`SELECT secret_encrypted FROM ai_provider WHERE id = ?`, id).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if plain, err := e.settings.aiDecrypt(secret); err != nil || plain != adminAISecret {
		t.Fatalf("empty secret did not keep the stored one: %q (err %v)", plain, err)
	}
	res, body = e.postForm("/admin/ai/provider/"+id+"/change/",
		aiProviderForm("http://127.0.0.1:7001", map[string]string{"clear_secret": "on"}))
	e.wantStatus(res, http.StatusFound, "clear secret", body)
	if err := e.db.QueryRow(`SELECT secret_encrypted FROM ai_provider WHERE id = ?`, id).Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if len(secret) != 0 {
		t.Fatalf("clear_secret left a stored secret")
	}
}

func TestAdminProviderInvalidSettingsAreFormErrors(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	cases := []struct {
		fields map[string]string
		error  string
	}{
		{map[string]string{"style_config": `{"path": "no-slash"}`}, "start with /"},
		{map[string]string{"base_url": "ftp://x.example"}, "http:// or https://"},
		{map[string]string{"auth_header": "Content-Type"}, "set by Doory"},
		{map[string]string{"secret": "a\nb"}, "line breaks"},
	}
	for _, testCase := range cases {
		res, body := e.postForm("/admin/ai/provider/add/", aiProviderForm("http://127.0.0.1:7001", testCase.fields))
		e.wantStatus(res, http.StatusOK, "invalid "+testCase.error, body)
		if !strings.Contains(body, testCase.error) {
			t.Fatalf("error %q not shown: %.600s", testCase.error, body)
		}
		if e.count(`SELECT COUNT(*) FROM ai_provider`) != 0 {
			t.Fatalf("a provider was created from invalid settings %v", testCase.fields)
		}
	}
}

func TestAdminProviderDetailActionsRenderConfirmations(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	res, body := e.postForm("/admin/ai/provider/add/", aiProviderForm("http://127.0.0.1:7001", nil))
	e.wantStatus(res, http.StatusFound, "add provider", body)
	var id string
	if err := e.db.QueryRow(`SELECT id FROM ai_provider`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Unfold's URL paths are the method names: load_models / test_connection.
	page := e.mustGet("/admin/ai/provider/" + id + "/load_models/")
	if !strings.Contains(page, "Load models") {
		t.Fatalf("load models confirmation missing")
	}
	if e.count(`SELECT COUNT(*) FROM ai_providermodel`) != 0 {
		t.Fatalf("the GET confirmation created models")
	}
	page = e.mustGet("/admin/ai/provider/" + id + "/test_connection/")
	if !strings.Contains(page, "Test connection") {
		t.Fatalf("test connection confirmation missing")
	}
	// Running either action needs the G9 transport; until then the POST reports
	// that the route is registered but not ported (never a silent no-op).
	res, body = e.postForm("/admin/ai/provider/"+id+"/load_models/", url.Values{})
	e.wantStatus(res, http.StatusFound, "load models run", body)
	flash := e.mustGet(res.Header.Get("Location"))
	if !strings.Contains(flash, "route registered but not ported") {
		t.Fatalf("load models did not report the pending transport: %.300s", flash)
	}
}

func (e *adminEnv) makeProvider(baseURL, ownerID string, task string) string {
	e.t.Helper()
	id := newUUID4Hex()
	now := e.now()
	e.exec(`INSERT INTO ai_provider
		(id, created_at, updated_at, name, base_url, style, style_config, auth_header,
		 secret_encrypted, tasks, timeout_seconds, max_image_bytes, sends_offsite, enabled,
		 sort_order, audience, daily_quota, owner_id)
		VALUES (?, ?, ?, 'Server X', ?, 'doory', '{}', '', ?, ?, 60, 0, FALSE, TRUE, 0,
		 'everyone', 0, ?)`,
		id, now, now, baseURL, []byte{}, pyJSONStringList([]string{task}), nullIfEmpty(ownerID))
	return id
}

func TestAdminUserServersListedWithoutSecrets(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	owner := e.makeUser("owner@example.com", false, false)
	provider := e.makeProvider("http://127.0.0.1:7001", owner.ID, "background-removal")
	encrypted, err := e.settings.aiEncrypt(adminAISecret)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE ai_provider SET secret_encrypted = ?, auth_header = 'Authorization' WHERE id = ?`,
		[]byte(encrypted), provider)

	// Operator providers never show a user's server.
	res, body := e.get("/admin/ai/provider/" + provider + "/change/")
	e.wantStatus(res, http.StatusFound, "operator change of a user server", body)

	page := e.mustGet("/admin/ai/userprovider/" + provider + "/change/")
	if !strings.Contains(page, "owner@example.com") {
		t.Fatalf("owner email missing from the user provider page")
	}
	if strings.Contains(page, "admin-secret-value") {
		t.Fatalf("the user provider secret reached the page")
	}
	listing := e.mustGet("/admin/ai/userprovider/")
	if !strings.Contains(listing, "Server X") {
		t.Fatalf("the user server is missing from the user provider changelist")
	}
}

func TestAdminCallLogIsReadOnly(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	caller := e.makeUser("caller@example.com", false, false)
	now := e.now()
	e.exec(`INSERT INTO ai_aicall
		(id, created_at, updated_at, provider_ref, provider_name, model, task, status, error_code,
		 duration_ms, bytes_in, bytes_out, user_id, provider_id)
		VALUES (?, ?, ?, 'env-1', '', '', 'background-removal', 'ok', '', 10, 0, 0, ?, NULL)`,
		newUUID4Hex(), now, now, caller.ID)
	page := e.mustGet("/admin/ai/aicall/?status__exact=ok")
	if !strings.Contains(page, "caller@example.com") {
		t.Fatalf("the caller is missing from the filtered call log")
	}
	res, body := e.get("/admin/ai/aicall/add/")
	e.wantStatus(res, http.StatusForbidden, "aicall add", body)
}

func TestAdminEnvProvidersPanel(t *testing.T) {
	e := newAdminEnv(t)
	e.adminUser()
	const yKey = "y-key-secret-marker-987"
	e.settings.AIProviderEnvProvider = []AIEnvProvider{
		{
			index: "1", URL: "http://127.0.0.1:7001", STYLE: "multipart",
			STYLE_CONFIG: `{"path": "/v1/remove-background", "file_field": "image_file"}`,
			AUTH_HEADER:  "X-Api-Key", AUTH_VALUE: yKey, LABEL: "Model server",
		},
		{index: "2", URL: "nope"},
	}
	page := e.mustGet("/admin/ai/provider/")
	if !strings.Contains(page, "env-1") || !strings.Contains(page, "Model server") {
		t.Fatalf("the environment provider row is missing: %.500s", page)
	}
	if !strings.Contains(page, "AI_PROVIDER_2_URL") {
		t.Fatalf("the invalid environment row was not reported")
	}
	if strings.Contains(page, yKey) || strings.Contains(page, "127.0.0.1:7001") {
		t.Fatalf("the environment provider leaked its secret or base URL")
	}
	if !strings.Contains(page, `<form method="post"`) {
		t.Fatalf("the environment row has no test form")
	}

	// The test button runs through the G9 transport upstream; until it lands
	// the POST reports the pending route and still never echoes the secret.
	res, body := e.postForm("/admin/ai/provider/env/env-1/test/", url.Values{})
	e.wantStatus(res, http.StatusOK, "env test run", body)
	if strings.Contains(body, yKey) {
		t.Fatalf("the env test result leaked the secret")
	}
	res, body = e.postForm("/admin/ai/provider/env/env-9/test/", url.Values{})
	e.wantStatus(res, http.StatusNotFound, "unknown env test", body)
}

func TestAdminEnvProviderTestNeedsChangePermission(t *testing.T) {
	e := newAdminEnv(t)
	plain := e.makeUser("plain@example.com", true, false)
	e.loginAs(plain)
	res, body := e.get("/admin/ai/provider/env/env-1/test/")
	e.wantStatus(res, http.StatusForbidden, "env test GET", body)
	res, body = e.postForm("/admin/ai/provider/env/env-1/test/", url.Values{})
	e.wantStatus(res, http.StatusForbidden, "env test POST", body)
}
