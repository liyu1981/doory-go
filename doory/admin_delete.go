// Admin delete cascades for the models whose Django relations are CASCADE.
//
// The migration DDL has no ON DELETE clauses: Django's collector removes the
// related rows itself, so the Go admin has to do the same before deleting the
// parent or a foreign key blocks the row.
package doory

import "strings"

// adminPlaceholders renders n positional placeholders.
func adminPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// adminDeleteComments removes comments and their reply subtrees, children
// first (matching Django's parent FK on_delete=CASCADE).
func (a *App) adminDeleteComments(ids []string) error {
	seen := map[string]bool{}
	var ordered []string
	queue := append([]string(nil), ids...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ordered = append(ordered, id)
		rows, err := a.DB.Query(`SELECT id FROM sharing_comment WHERE parent_id = ?`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var child string
			if err := rows.Scan(&child); err != nil {
				rows.Close()
				return err
			}
			queue = append(queue, child)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for i := len(ordered) - 1; i >= 0; i-- {
		if _, err := a.DB.Exec(`DELETE FROM sharing_comment WHERE id = ?`, ordered[i]); err != nil {
			return err
		}
	}
	return nil
}

// adminDeleteUserCascade ports the collector for deleting an accounts.User:
// owned designs/assets/folders/brand kits/tokens/fonts/providers go away,
// nullable creator references are cleared, and the files those rows point at
// are removed.
func (a *App) adminDeleteUserCascade(actor *UserRow, id string) error {
	// Designs (and their versions, comments, share links, collab rows and
	// thumbnails) first — purgeDesigns already walks the same cascade.
	rows, err := a.DB.Query(`SELECT id FROM designs_design WHERE owner_id = ?`, id)
	if err != nil {
		return err
	}
	var designIDs []string
	for rows.Next() {
		var designID string
		if err := rows.Scan(&designID); err != nil {
			rows.Close()
			return err
		}
		designIDs = append(designIDs, designID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(designIDs) > 0 {
		if _, err := a.purgeDesigns(designIDs); err != nil {
			return err
		}
		a.notifyDesignsRecheck(designIDs)
	}

	// Comments the user wrote on other people's designs; replies cascade.
	rows, err = a.DB.Query(`SELECT id FROM sharing_comment WHERE author_id = ?`, id)
	if err != nil {
		return err
	}
	var commentIDs []string
	for rows.Next() {
		var commentID string
		if err := rows.Scan(&commentID); err != nil {
			rows.Close()
			return err
		}
		commentIDs = append(commentIDs, commentID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if err := a.adminDeleteComments(commentIDs); err != nil {
		return err
	}

	// Assets, their files and their brand-kit join rows.
	type assetFiles struct {
		id                                 string
		file, thumbnail, poster, filmstrip string
		audio                              string
	}
	rows, err = a.DB.Query(`SELECT id, file, thumbnail, poster, filmstrip, audio
		FROM assets_asset WHERE owner_id = ?`, id)
	if err != nil {
		return err
	}
	var assets []assetFiles
	for rows.Next() {
		var asset assetFiles
		if err := rows.Scan(&asset.id, &asset.file, &asset.thumbnail, &asset.poster,
			&asset.filmstrip, &asset.audio); err != nil {
			rows.Close()
			return err
		}
		assets = append(assets, asset)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(assets) > 0 {
		assetIDs := make([]string, 0, len(assets))
		for _, asset := range assets {
			assetIDs = append(assetIDs, asset.id)
		}
		q := `DELETE FROM brand_brandkit_logos WHERE asset_id IN (` + adminPlaceholders(len(assetIDs)) + `)`
		if _, err := a.DB.Exec(q, adminStrings(assetIDs)...); err != nil {
			return err
		}
		if _, err := a.DB.Exec(`DELETE FROM assets_asset WHERE owner_id = ?`, id); err != nil {
			return err
		}
		for _, asset := range assets {
			for _, name := range []string{asset.file, asset.thumbnail, asset.poster,
				asset.filmstrip, asset.audio} {
				a.deleteMediaFile(name)
			}
		}
	}

	// Brand kits and their logo joins.
	_, _ = a.DB.Exec(`DELETE FROM brand_brandkit_logos WHERE brandkit_id IN
		(SELECT id FROM brand_brandkit WHERE owner_id = ?)`, id)
	if _, err := a.DB.Exec(`DELETE FROM brand_brandkit WHERE owner_id = ?`, id); err != nil {
		return err
	}

	// Font families owned by the user (variants and files cascade).
	rows, err = a.DB.Query(`SELECT v.file FROM fonts_fontvariant v
		JOIN fonts_fontfamily f ON f.id = v.family_id WHERE f.owner_id = ?`, id)
	if err != nil {
		return err
	}
	var fontFiles []string
	for rows.Next() {
		var file string
		if err := rows.Scan(&file); err != nil {
			rows.Close()
			return err
		}
		fontFiles = append(fontFiles, file)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	_, _ = a.DB.Exec(`DELETE FROM fonts_fontvariant WHERE family_id IN
		(SELECT id FROM fonts_fontfamily WHERE owner_id = ?)`, id)
	if _, err := a.DB.Exec(`DELETE FROM fonts_fontfamily WHERE owner_id = ?`, id); err != nil {
		return err
	}
	for _, file := range fontFiles {
		a.deleteMediaFile(file)
	}

	// Provider rows the user owns; AICalls survive with provider_id cleared.
	rows, err = a.DB.Query(`SELECT id FROM ai_provider WHERE owner_id = ?`, id)
	if err != nil {
		return err
	}
	var providerIDs []string
	for rows.Next() {
		var providerID string
		if err := rows.Scan(&providerID); err != nil {
			rows.Close()
			return err
		}
		providerIDs = append(providerIDs, providerID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(providerIDs) > 0 {
		q := adminPlaceholders(len(providerIDs))
		args := adminStrings(providerIDs)
		if _, err := a.DB.Exec(`UPDATE ai_aicall SET provider_id = NULL WHERE provider_id IN (`+q+`)`, args...); err != nil {
			return err
		}
		_, _ = a.DB.Exec(`DELETE FROM ai_providermodel WHERE provider_id IN (`+q+`)`, args...)
		_, _ = a.DB.Exec(`DELETE FROM ai_provider_allowed_users WHERE provider_id IN (`+q+`)`, args...)
		if _, err := a.DB.Exec(`DELETE FROM ai_provider WHERE id IN (`+q+`)`, args...); err != nil {
			return err
		}
	}

	// Rows owned directly by the user.
	steps := []string{
		`DELETE FROM ai_provider_allowed_users WHERE user_id = ?`,
		`DELETE FROM ai_aicall WHERE user_id = ?`,
		`DELETE FROM apitokens_apitoken WHERE owner_id = ?`,
		`DELETE FROM accounts_oidcidentity WHERE user_id = ?`,
		`DELETE FROM accounts_user_groups WHERE user_id = ?`,
		`DELETE FROM accounts_user_user_permissions WHERE user_id = ?`,
		`DELETE FROM mcp_callwindow WHERE user_id = ?`,
		`DELETE FROM presets_sizechoices WHERE user_id = ?`,
		`DELETE FROM collab_roomconnection WHERE user_id = ?`,
		`DELETE FROM django_admin_log WHERE user_id = ?`,
	}
	for _, stmt := range steps {
		if _, err := a.DB.Exec(stmt, id); err != nil {
			return err
		}
	}

	// Nullable creator references.
	nullSteps := []string{
		`UPDATE designs_designversion SET created_by_id = NULL WHERE created_by_id = ?`,
		`UPDATE sharing_sharelink SET created_by_id = NULL WHERE created_by_id = ?`,
		`UPDATE gallery_template SET created_by_id = NULL WHERE created_by_id = ?`,
		`UPDATE collab_ystate SET last_editor_id = NULL WHERE last_editor_id = ?`,
	}
	for _, stmt := range nullSteps {
		if _, err := a.DB.Exec(stmt, id); err != nil {
			return err
		}
	}

	// Folders owned by the user; designs and child folders of other owners
	// survive with the nullable reference cleared (Django SET_NULL).
	_, _ = a.DB.Exec(`UPDATE designs_design SET folder_id = NULL
		WHERE folder_id IN (SELECT id FROM designs_folder WHERE owner_id = ?)`, id)
	_, _ = a.DB.Exec(`UPDATE designs_folder SET parent_id = NULL
		WHERE parent_id IN (SELECT id FROM designs_folder WHERE owner_id = ?)`, id)
	if _, err := a.DB.Exec(`DELETE FROM designs_folder WHERE owner_id = ?`, id); err != nil {
		return err
	}

	var avatar string
	_ = a.DB.QueryRow(`SELECT avatar FROM accounts_user WHERE id = ?`, id).Scan(&avatar)
	if _, err := a.DB.Exec(`DELETE FROM accounts_user WHERE id = ?`, id); err != nil {
		return err
	}
	if avatar != "" {
		a.deleteMediaFile(avatar)
	}
	_ = actor
	return nil
}

func adminStrings(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// adminDeleteFolder clears the nullable references before deleting a folder:
// designs point at it (SET_NULL) and child folders have parent_id (SET_NULL).
func (a *App) adminDeleteFolder(id string) error {
	if _, err := a.DB.Exec(`UPDATE designs_design SET folder_id = NULL WHERE folder_id = ?`, id); err != nil {
		return err
	}
	if _, err := a.DB.Exec(`UPDATE designs_folder SET parent_id = NULL WHERE parent_id = ?`, id); err != nil {
		return err
	}
	_, err := a.DB.Exec(`DELETE FROM designs_folder WHERE id = ?`, id)
	return err
}
