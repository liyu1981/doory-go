// MCP registry: apps/mcp/registry.py (TOOL list, @tool guarded wrapper with
// scope checks + per-user call rate limit + error translation; RESOURCES /
// PROMPTS lists). Tool bodies land in the next slice; this slice registers
// the probed tool surface.

package doory

// Scopes mirror apps/apitokens/scopes.py values.
const (
	McpScopeDesignsRead    = "designs:read"
	McpScopeDesignsWrite   = "designs:write"
	McpScopeDesignsTrash   = "designs:trash"
	McpScopeAssetsWrite    = "assets:write"
	McpScopeCommentsWrite  = "comments:write"
	McpScopeCommentsManage = "comments:manage"
	McpScopeBrandWrite     = "brand:write"
	McpScopeSharingWrite   = "sharing:write"
)

// mcpAnnotations is the ToolAnnotations wire shape.
type mcpAnnotations struct {
	Title           string `json:"title"`
	ReadOnlyHint    bool   `json:"readOnlyHint"`
	DestructiveHint bool   `json:"destructiveHint"`
	IdempotentHint  bool   `json:"idempotentHint"`
	OpenWorldHint   bool   `json:"openWorldHint"`
}

// mcpToolDef is one probed MCP tool (registry.ToolSpec).
type mcpToolDef struct {
	Name        string
	Title       string
	Description string
	Scope       string
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Annotations mcpAnnotations
}

// mcpToolError carries the guarded-wrapper error text (registry.ToolError).
type mcpToolError struct {
	Message string
}

func (e *mcpToolError) Error() string { return e.Message }

// mcpRateWindow mirrors the mcp_callwindow table (one row per user, one
// window minute): minute index = int(time.Now().UTC().Unix() / 60).
type mcpRateWindow struct {
	UserID string
	Window int64
	Calls  int
}

// mcpToolDefs returns the probed MCP tool surface (registry.TOOLS) as wire
// specs; handlers are attached by the transport (tool bodies in a later slice).
func mcpToolDefs() []McpToolSpec {
	defs := []mcpToolDef{
		{Name: "list_assets", Title: "List uploads", Description: "Your uploaded pictures, newest first. Place one with edit_design:\n    {\"op\": \"add_image\", \"asset_id\": …}.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List uploads", ReadOnlyHint: true}},
		{Name: "search_stock_photos", Title: "Search stock photos", Description: "Search free stock photos. Use import_stock_photo to add one to your uploads.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Search stock photos", ReadOnlyHint: true}},
		{Name: "import_stock_photo", Title: "Import a stock photo", Description: "Copy a stock photo into your uploads (once; importing it again adds nothing new).", Scope: McpScopeAssetsWrite},
		{Name: "upload_image_from_url", Title: "Upload an image from a URL", Description: "Copy an image from a public https URL into your uploads.", Scope: McpScopeAssetsWrite},
		{Name: "search_audio", Title: "Search sounds", Description: "Search sounds on Freesound (music, sound effects, ambience).", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Search sounds", ReadOnlyHint: true}},
		{Name: "import_audio", Title: "Import a sound", Description: "Copy a Freesound sound into your audio uploads (once; importing it again returns the same asset).", Scope: McpScopeAssetsWrite, Idempotent: true},
		{Name: "get_design_credits", Title: "Get a design's credits", Description: "The credits a design owes for the sounds it uses. `credits_text` lists the ones that must be paid.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Get a design's credits", ReadOnlyHint: true}},
		{Name: "list_designs", Title: "List designs", Description: "List your designs, most recently changed first.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List designs", ReadOnlyHint: true}},
		{Name: "get_design", Title: "Read a design", Description: "Read a design: its pages and, on each page, every object (with ids/bounds); pass include_document for the full document JSON.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Read a design", ReadOnlyHint: true}},
		{Name: "list_fields", Title: "List template fields", Description: "The template fields of a design: objects tagged with a field key.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List template fields", ReadOnlyHint: true}},
		{Name: "create_design", Title: "Create a design", Description: "Create a design: from a template, or blank at the given size.", Scope: McpScopeDesignsWrite},
		{Name: "rename_design", Title: "Rename a design", Description: "Change a design's title.", Scope: McpScopeDesignsWrite, Idempotent: true},
		{Name: "duplicate_design", Title: "Duplicate a design", Description: "Copy a design; the copy is a new design you can change without touching the original.", Scope: McpScopeDesignsWrite},
		{Name: "edit_design", Title: "Edit a design", Description: "Change a design with a list of operations, applied in order.", Scope: McpScopeDesignsWrite},
		{Name: "fill_design", Title: "Fill template fields", Description: "Fill a template's fields by key in one step: every object tagged with a field gets a value.", Scope: McpScopeDesignsWrite},
		{Name: "bulk_fill", Title: "Bulk create from a template", Description: "Make one new design per row from a template, filled like fill_design.", Scope: McpScopeDesignsWrite},
		{Name: "list_ai_models", Title: "List AI models", Description: "The AI models you can pick a task. 'device' models run in the user's browser and cannot be used server-side.", Scope: McpScopeAssetsWrite, ReadOnly: true, Annotations: mcpAnnotations{Title: "List AI models", ReadOnlyHint: true}},
		{Name: "remove_asset_background", Title: "Remove background from an upload", Description: "Remove the background of a picture in your uploads; the result is a new asset.", Scope: McpScopeAssetsWrite},
		{Name: "list_brand_kits", Title: "List brand kits", Description: "Your brand kits: named colours, font roles and logos (logo ids work as asset_id in add_image).", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List brand kits", ReadOnlyHint: true}},
		{Name: "create_brand_kit", Title: "Create a brand kit", Description: "Create a brand kit. The default kit is the one the editor of the design uses.", Scope: McpScopeBrandWrite},
		{Name: "update_brand_kit", Title: "Change a brand kit", Description: "Change a brand kit. Only what you pass changes; colours, fonts, logo ids.", Scope: McpScopeBrandWrite, Idempotent: true},
		{Name: "list_templates", Title: "List templates", Description: "Find templates to start a design from (create_design with `template`).", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List templates", ReadOnlyHint: true}},
		{Name: "search_elements", Title: "Search elements", Description: "Search the graphics catalogue (icons, illustrations, stickers, shapes, lines, frames, graphics).", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Search elements", ReadOnlyHint: true}},
		{Name: "search_fonts", Title: "Search fonts", Description: "Search the font catalogue, most popular first. Use `family` to narrow.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Search fonts", ReadOnlyHint: true}},
		{Name: "list_comments", Title: "List comments", Description: "Comments on a design, oldest first. A comment with x, y is pinned to a point.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List comments", ReadOnlyHint: true}},
		{Name: "add_comment", Title: "Add a comment", Description: "Comment on a design, optionally pinned to a point (x, y) of it.", Scope: McpScopeCommentsWrite},
		{Name: "resolve_comment", Title: "Resolve a comment", Description: "Mark a comment thread as resolved (or re-open it with resolved = false).", Scope: McpScopeCommentsManage, Idempotent: true, Annotations: mcpAnnotations{Title: "Resolve a comment", IdempotentHint: true}},
		{Name: "edit_comment", Title: "Edit a comment", Description: "Change the text of a comment you wrote (comments by other people can be resolved, not rewritten).", Scope: McpScopeCommentsManage, Idempotent: true, Annotations: mcpAnnotations{Title: "Edit a comment", IdempotentHint: true}},
		{Name: "delete_comment", Title: "Delete a comment", Description: "Delete a comment, with its replies. This cannot be undone; prefer resolve_comment for finished threads.", Scope: McpScopeCommentsManage, Destructive: true, Idempotent: true, Annotations: mcpAnnotations{Title: "Delete a comment", DestructiveHint: true, IdempotentHint: true}},
		{Name: "export_design", Title: "Export a design as an image", Description: "Render one page of a design as an image, exactly as the editor draws it.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "Export a design as an image", ReadOnlyHint: true}},
		{Name: "list_versions", Title: "List versions", Description: "A design's saved versions, newest first (automatic snapshots + manual restores).", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List versions", ReadOnlyHint: true}},
		{Name: "restore_version", Title: "Restore a version", Description: "Put a design back to a saved version (the current state is kept as a new version).", Scope: McpScopeDesignsTrash},
		{Name: "list_folders", Title: "List folders", Description: "Your folders, by name. A folder with a parent_id is inside another folder.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List folders", ReadOnlyHint: true}},
		{Name: "create_folder", Title: "Create a folder", Description: "Create a folder, at the top level or inside another folder.", Scope: McpScopeDesignsWrite},
		{Name: "rename_folder", Title: "Rename a folder", Description: "Change a folder's name.", Scope: McpScopeDesignsWrite, Idempotent: true, Annotations: mcpAnnotations{Title: "Rename a folder", IdempotentHint: true}},
		{Name: "move_folder", Title: "Move a folder", Description: "Put a folder inside another folder, or back at the top level.", Scope: McpScopeDesignsWrite},
		{Name: "delete_folder", Title: "Delete a folder", Description: "Delete a folder. Nothing in it is deleted: its designs and subfolders are moved up a level.", Scope: McpScopeDesignsTrash, Destructive: true, Idempotent: true, Annotations: mcpAnnotations{Title: "Delete a folder", DestructiveHint: true, IdempotentHint: true}},
		{Name: "move_design", Title: "Move a design to a folder", Description: "Put a design in a folder, or back at the top level.", Scope: McpScopeDesignsWrite},
		{Name: "star_design", Title: "Star a design", Description: "Star a design (or un-star it with starred = false); list_designs?starred=true filters.", Scope: McpScopeDesignsWrite, Idempotent: true},
		{Name: "trash_design", Title: "Move a design to the trash", Description: "Move a design to the trash. It can be brought back with restore_design.", Scope: McpScopeDesignsTrash, Destructive: true, Idempotent: true, Annotations: mcpAnnotations{Title: "Move a design to the trash", DestructiveHint: true, IdempotentHint: true}},
		{Name: "restore_design", Title: "Restore a design", Description: "Bring a trashed design back.", Scope: McpScopeDesignsTrash},
		{Name: "list_share_links", Title: "List share links", Description: "A design's share links, newest first, with their URL, role and expiry.", Scope: McpScopeDesignsRead, ReadOnly: true, Annotations: mcpAnnotations{Title: "List share links", ReadOnlyHint: true}},
		{Name: "create_share_link", Title: "Create a share link", Description: "Create a link that lets anyone who has it view, comment on or edit a design.", Scope: McpScopeSharingWrite, Destructive: true},
		{Name: "update_share_link", Title: "Change a share link", Description: "Change a share link. Only what you pass changes.", Scope: McpScopeSharingWrite},
		{Name: "revoke_share_link", Title: "Delete a share link", Description: "Delete a share link: it stops working at once for everyone who has it.", Scope: McpScopeSharingWrite, Destructive: true},
	}
	specs := make([]McpToolSpec, 0, len(defs))
	for i := range defs {
		d := defs[i]
		spec := McpToolSpec{Name: d.Name, Title: d.Title, Description: d.Description, Scope: d.Scope, ReadOnly: d.ReadOnly, Destructive: d.Destructive, Idempotent: d.Idempotent}
		spec.Annotations.Title = d.Annotations.Title
		spec.Annotations.ReadOnlyHint = d.Annotations.ReadOnlyHint
		spec.Annotations.DestructiveHint = d.Annotations.DestructiveHint
		spec.Annotations.IdempotentHint = d.Annotations.IdempotentHint
		spec.Annotations.OpenWorldHint = d.Annotations.OpenWorldHint
		specs = append(specs, spec)
	}
	return specs
}

// mcpResources returns the probed MCP resources (registry.RESOURCES):
// doory://designs/{design_id} scope designs:read.
func mcpResources() []McpResourceSpec {
	return []McpResourceSpec{
		{URI: "doory://designs/{design_id}", Name: "Design", Title: "Design", MimeType: "application/json", Scope: McpScopeDesignsRead},
	}
}

// mcpPrompts returns the probed MCP prompts (registry.PROMPTS).
func mcpPrompts() []McpPromptSpec {
	return []McpPromptSpec{}
}
