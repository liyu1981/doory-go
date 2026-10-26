// Auth rows Django's post_migrate signal creates: django_content_type and
// auth_permission. The oracle has 33 content types and 132 permissions at the
// pin; admin pages, auth_permission checks and django_admin_log all depend on
// them. Seeding is idempotent and runs at the end of `Migrate`, like
// django.contrib.contenttypes/auth's post_migrate receivers.
package doory

import (
	"context"
	"database/sql"
	"fmt"
)

type adminContentTypeSeed struct {
	App   string
	Model string
}

// adminContentTypeSeeds is the oracle's content-type list in creation order.
// Not every entry is an admin-registered model (sessions, collab, mcp, …);
// Django still creates their content types and permissions.
var adminContentTypeSeeds = []adminContentTypeSeed{
	{"admin", "logentry"},
	{"auth", "group"},
	{"auth", "permission"},
	{"contenttypes", "contenttype"},
	{"sessions", "session"},
	{"accounts", "oidcidentity"},
	{"accounts", "user"},
	{"designs", "design"},
	{"designs", "designversion"},
	{"designs", "folder"},
	{"assets", "asset"},
	{"gallery", "template"},
	{"gallery", "templatecategory"},
	{"elements", "element"},
	{"elements", "elementpack"},
	{"fonts", "fontfamily"},
	{"fonts", "fontvariant"},
	{"brand", "brandkit"},
	{"sharing", "comment"},
	{"sharing", "sharelink"},
	{"collab", "collabinstance"},
	{"collab", "roomconnection"},
	{"collab", "ystate"},
	{"collab", "yupdate"},
	{"presets", "sizechoices"},
	{"presets", "sizepreset"},
	{"presets", "sizepresetgroup"},
	{"apitokens", "apitoken"},
	{"mcp", "callwindow"},
	{"ai", "aicall"},
	{"ai", "provider"},
	{"ai", "providermodel"},
	{"ai", "userprovider"},
}

type adminPermissionSeed struct {
	App      string
	Model    string
	Codename string
	Name     string
}

// adminPermissionSeeds is the oracle's auth_permission list in creation
// order: add, change, delete, view per model (model order may differ from the
// content-type order because apps register models in their own order).
var adminPermissionSeeds = []adminPermissionSeed{
	{"admin", "logentry", "add_logentry", "Can add log entry"},
	{"admin", "logentry", "change_logentry", "Can change log entry"},
	{"admin", "logentry", "delete_logentry", "Can delete log entry"},
	{"admin", "logentry", "view_logentry", "Can view log entry"},
	{"auth", "permission", "add_permission", "Can add permission"},
	{"auth", "permission", "change_permission", "Can change permission"},
	{"auth", "permission", "delete_permission", "Can delete permission"},
	{"auth", "permission", "view_permission", "Can view permission"},
	{"auth", "group", "add_group", "Can add group"},
	{"auth", "group", "change_group", "Can change group"},
	{"auth", "group", "delete_group", "Can delete group"},
	{"auth", "group", "view_group", "Can view group"},
	{"contenttypes", "contenttype", "add_contenttype", "Can add content type"},
	{"contenttypes", "contenttype", "change_contenttype", "Can change content type"},
	{"contenttypes", "contenttype", "delete_contenttype", "Can delete content type"},
	{"contenttypes", "contenttype", "view_contenttype", "Can view content type"},
	{"sessions", "session", "add_session", "Can add session"},
	{"sessions", "session", "change_session", "Can change session"},
	{"sessions", "session", "delete_session", "Can delete session"},
	{"sessions", "session", "view_session", "Can view session"},
	{"accounts", "user", "add_user", "Can add user"},
	{"accounts", "user", "change_user", "Can change user"},
	{"accounts", "user", "delete_user", "Can delete user"},
	{"accounts", "user", "view_user", "Can view user"},
	{"accounts", "oidcidentity", "add_oidcidentity", "Can add oidc identity"},
	{"accounts", "oidcidentity", "change_oidcidentity", "Can change oidc identity"},
	{"accounts", "oidcidentity", "delete_oidcidentity", "Can delete oidc identity"},
	{"accounts", "oidcidentity", "view_oidcidentity", "Can view oidc identity"},
	{"designs", "folder", "add_folder", "Can add folder"},
	{"designs", "folder", "change_folder", "Can change folder"},
	{"designs", "folder", "delete_folder", "Can delete folder"},
	{"designs", "folder", "view_folder", "Can view folder"},
	{"designs", "design", "add_design", "Can add design"},
	{"designs", "design", "change_design", "Can change design"},
	{"designs", "design", "delete_design", "Can delete design"},
	{"designs", "design", "view_design", "Can view design"},
	{"designs", "designversion", "add_designversion", "Can add design version"},
	{"designs", "designversion", "change_designversion", "Can change design version"},
	{"designs", "designversion", "delete_designversion", "Can delete design version"},
	{"designs", "designversion", "view_designversion", "Can view design version"},
	{"assets", "asset", "add_asset", "Can add asset"},
	{"assets", "asset", "change_asset", "Can change asset"},
	{"assets", "asset", "delete_asset", "Can delete asset"},
	{"assets", "asset", "view_asset", "Can view asset"},
	{"gallery", "templatecategory", "add_templatecategory", "Can add template category"},
	{"gallery", "templatecategory", "change_templatecategory", "Can change template category"},
	{"gallery", "templatecategory", "delete_templatecategory", "Can delete template category"},
	{"gallery", "templatecategory", "view_templatecategory", "Can view template category"},
	{"gallery", "template", "add_template", "Can add template"},
	{"gallery", "template", "change_template", "Can change template"},
	{"gallery", "template", "delete_template", "Can delete template"},
	{"gallery", "template", "view_template", "Can view template"},
	{"elements", "elementpack", "add_elementpack", "Can add element pack"},
	{"elements", "elementpack", "change_elementpack", "Can change element pack"},
	{"elements", "elementpack", "delete_elementpack", "Can delete element pack"},
	{"elements", "elementpack", "view_elementpack", "Can view element pack"},
	{"elements", "element", "add_element", "Can add element"},
	{"elements", "element", "change_element", "Can change element"},
	{"elements", "element", "delete_element", "Can delete element"},
	{"elements", "element", "view_element", "Can view element"},
	{"fonts", "fontfamily", "add_fontfamily", "Can add font family"},
	{"fonts", "fontfamily", "change_fontfamily", "Can change font family"},
	{"fonts", "fontfamily", "delete_fontfamily", "Can delete font family"},
	{"fonts", "fontfamily", "view_fontfamily", "Can view font family"},
	{"fonts", "fontvariant", "add_fontvariant", "Can add font variant"},
	{"fonts", "fontvariant", "change_fontvariant", "Can change font variant"},
	{"fonts", "fontvariant", "delete_fontvariant", "Can delete font variant"},
	{"fonts", "fontvariant", "view_fontvariant", "Can view font variant"},
	{"brand", "brandkit", "add_brandkit", "Can add brand kit"},
	{"brand", "brandkit", "change_brandkit", "Can change brand kit"},
	{"brand", "brandkit", "delete_brandkit", "Can delete brand kit"},
	{"brand", "brandkit", "view_brandkit", "Can view brand kit"},
	{"sharing", "comment", "add_comment", "Can add comment"},
	{"sharing", "comment", "change_comment", "Can change comment"},
	{"sharing", "comment", "delete_comment", "Can delete comment"},
	{"sharing", "comment", "view_comment", "Can view comment"},
	{"sharing", "sharelink", "add_sharelink", "Can add share link"},
	{"sharing", "sharelink", "change_sharelink", "Can change share link"},
	{"sharing", "sharelink", "delete_sharelink", "Can delete share link"},
	{"sharing", "sharelink", "view_sharelink", "Can view share link"},
	{"collab", "collabinstance", "add_collabinstance", "Can add collab instance"},
	{"collab", "collabinstance", "change_collabinstance", "Can change collab instance"},
	{"collab", "collabinstance", "delete_collabinstance", "Can delete collab instance"},
	{"collab", "collabinstance", "view_collabinstance", "Can view collab instance"},
	{"collab", "ystate", "add_ystate", "Can add y state"},
	{"collab", "ystate", "change_ystate", "Can change y state"},
	{"collab", "ystate", "delete_ystate", "Can delete y state"},
	{"collab", "ystate", "view_ystate", "Can view y state"},
	{"collab", "roomconnection", "add_roomconnection", "Can add room connection"},
	{"collab", "roomconnection", "change_roomconnection", "Can change room connection"},
	{"collab", "roomconnection", "delete_roomconnection", "Can delete room connection"},
	{"collab", "roomconnection", "view_roomconnection", "Can view room connection"},
	{"collab", "yupdate", "add_yupdate", "Can add y update"},
	{"collab", "yupdate", "change_yupdate", "Can change y update"},
	{"collab", "yupdate", "delete_yupdate", "Can delete y update"},
	{"collab", "yupdate", "view_yupdate", "Can view y update"},
	{"presets", "sizepresetgroup", "add_sizepresetgroup", "Can add size preset group"},
	{"presets", "sizepresetgroup", "change_sizepresetgroup", "Can change size preset group"},
	{"presets", "sizepresetgroup", "delete_sizepresetgroup", "Can delete size preset group"},
	{"presets", "sizepresetgroup", "view_sizepresetgroup", "Can view size preset group"},
	{"presets", "sizepreset", "add_sizepreset", "Can add size preset"},
	{"presets", "sizepreset", "change_sizepreset", "Can change size preset"},
	{"presets", "sizepreset", "delete_sizepreset", "Can delete size preset"},
	{"presets", "sizepreset", "view_sizepreset", "Can view size preset"},
	{"presets", "sizechoices", "add_sizechoices", "Can add size choices"},
	{"presets", "sizechoices", "change_sizechoices", "Can change size choices"},
	{"presets", "sizechoices", "delete_sizechoices", "Can delete size choices"},
	{"presets", "sizechoices", "view_sizechoices", "Can view size choices"},
	{"apitokens", "apitoken", "add_apitoken", "Can add api token"},
	{"apitokens", "apitoken", "change_apitoken", "Can change api token"},
	{"apitokens", "apitoken", "delete_apitoken", "Can delete api token"},
	{"apitokens", "apitoken", "view_apitoken", "Can view api token"},
	{"mcp", "callwindow", "add_callwindow", "Can add call window"},
	{"mcp", "callwindow", "change_callwindow", "Can change call window"},
	{"mcp", "callwindow", "delete_callwindow", "Can delete call window"},
	{"mcp", "callwindow", "view_callwindow", "Can view call window"},
	{"ai", "provider", "add_provider", "Can add provider"},
	{"ai", "provider", "change_provider", "Can change provider"},
	{"ai", "provider", "delete_provider", "Can delete provider"},
	{"ai", "provider", "view_provider", "Can view provider"},
	{"ai", "aicall", "add_aicall", "Can add AI call"},
	{"ai", "aicall", "change_aicall", "Can change AI call"},
	{"ai", "aicall", "delete_aicall", "Can delete AI call"},
	{"ai", "aicall", "view_aicall", "Can view AI call"},
	{"ai", "userprovider", "add_userprovider", "Can add user provider"},
	{"ai", "userprovider", "change_userprovider", "Can change user provider"},
	{"ai", "userprovider", "delete_userprovider", "Can delete user provider"},
	{"ai", "userprovider", "view_userprovider", "Can view user provider"},
	{"ai", "providermodel", "add_providermodel", "Can add provider model"},
	{"ai", "providermodel", "change_providermodel", "Can change provider model"},
	{"ai", "providermodel", "delete_providermodel", "Can delete provider model"},
	{"ai", "providermodel", "view_providermodel", "Can view provider model"},
}

var adminPermissionActions = []struct {
	Prefix string
	Verb   string
}{
	{"add", "add"},
	{"change", "change"},
	{"delete", "delete"},
	{"view", "view"},
}

// seedAuthRows ports django.contrib.contenttypes/auth's post_migrate
// receivers (create_contenttypes + create_permissions), idempotently.
func seedAuthRows(ctx context.Context, conn *sql.Conn) error {
	for _, ct := range adminContentTypeSeeds {
		var exists int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM django_content_type WHERE app_label = ? AND model = ?`,
			ct.App, ct.Model).Scan(&exists); err != nil {
			return fmt.Errorf("content type %s.%s: %w", ct.App, ct.Model, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO django_content_type (app_label, model) VALUES (?, ?)`,
			ct.App, ct.Model); err != nil {
			return fmt.Errorf("content type %s.%s: %w", ct.App, ct.Model, err)
		}
	}
	for _, perm := range adminPermissionSeeds {
		var ctID int64
		if err := conn.QueryRowContext(ctx,
			`SELECT id FROM django_content_type WHERE app_label = ? AND model = ?`,
			perm.App, perm.Model).Scan(&ctID); err != nil {
			return fmt.Errorf("permission %s.%s: content type: %w", perm.App, perm.Model, err)
		}
		var exists int
		if err := conn.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM auth_permission WHERE content_type_id = ? AND codename = ?`,
			ctID, perm.Codename).Scan(&exists); err != nil {
			return fmt.Errorf("permission %s: %w", perm.Codename, err)
		}
		if exists > 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO auth_permission (content_type_id, codename, name) VALUES (?, ?, ?)`,
			ctID, perm.Codename, perm.Name); err != nil {
			return fmt.Errorf("permission %s: %w", perm.Codename, err)
		}
	}
	return nil
}
