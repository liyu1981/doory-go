// App assembly: settings + db + lockout guard + route table + stack.
package doory

import (
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"time"
)

// App carries the process-wide kernel state.
type App struct {
	Settings *Settings
	DB       *sql.DB
	Router   *Router
	lockout  *lockoutGuard
	gate     *passwordGate

	// collab: the process-wide room registry (lazily built).
	collabOnce sync.Once
	collab     *CollabRoomRegistry

	// webapp: the embedded frontend + /static + /media (M14, family 14).
	webappOnce sync.Once
	webapp     *webappHandler

	// admin: the Django-admin-compatible site (D2, G27–G31).
	adminOnce sync.Once
	adminSite *adminSite
}

// NewApp builds the route table. M2 slice of the 214-route ledger
// (ledger/routes-v*.json): kernel + accounts auth. Route styles/modes come
// from the semantics table, never guesswork.
func NewApp(s *Settings, db *sql.DB) *App {
	a := &App{Settings: s, DB: db, lockout: newLockoutGuard(), gate: newPasswordGate()}
	rt := newRouter(a)
	rt.Register(
		Route{
			// @api_view(["GET"]) — Allow "GET, OPTIONS", auth classes []
			Path: "/api/v1/health/", Methods: []string{"GET"}, Style: AllowAPIView,
			Handler: a.health,
		},
		Route{
			Path: "/api/v1/config/", Methods: []string{"GET"}, Style: AllowAPIView,
			Handler: a.config,
		},
		Route{
			// accounts CsrfView: APIView subclass (Allow "GET, HEAD,
			// OPTIONS"); authentication_classes=[] — no session consult;
			// the cookie makes the response vary on Cookie (handler).
			Path: "/api/v1/auth/csrf/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.csrf,
		},
		Route{
			// RegisterView (AuthView): APIView subclass, CsrfProtectedMixin
			// (anonymous too), default auth classes consult the session.
			Path: "/api/v1/auth/register/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Handler: a.registerView,
		},
		Route{
			Path: "/api/v1/auth/login/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Handler: a.loginView,
		},
		Route{
			// GoogleLoginView (AuthView): AllowAny, CSRF always, default
			// session authentication. The credential is a Google ID token.
			Path: "/api/v1/auth/google/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireNever, Handler: a.googleLogin,
		},
		Route{
			Path: "/api/v1/auth/logout/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireAlways,
			Handler: a.logoutView,
		},
		Route{
			// MeView: Allow "GET, PATCH, DELETE, HEAD, OPTIONS" (probe0002
			// step13); GET is AllowAny, PATCH/DELETE IsAuthenticated; DELETE
			// is registered (Allow) but its handler slice (confirm_identity,
			// cascade) is family-pending — see views_auth.go.
			Path: "/api/v1/auth/me/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireUnsafe,
			Handler: a.meView,
		},
		Route{
			// OidcStartView / OidcCallbackView: public GETs (AllowAny) that
			// read the session (default SessionAuthentication -> Vary:
			// Cookie). Without a configured provider they redirect to
			// /login?sso_error=<code> (0.2.0; probes in test/parity).
			Path: "/api/v1/auth/oidc/start/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireNever,
			Handler: a.oidcStart,
		},
		Route{
			Path: "/api/v1/auth/oidc/callback/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireNever,
			Handler: a.oidcCallback,
		},
		Route{
			// OidcConnectionView (AuthView -> CsrfProtectedMixin): connect
			// (POST) and disconnect (DELETE), IsAuthenticated, CSRF always.
			Path: "/api/v1/auth/oidc/connection/", Methods: []string{"POST", "DELETE"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireAlways,
			Handler: a.oidcConnection,
		},
		Route{
			// PasswordReset/Confirm + EmailVerify (AuthView: AllowAny, CSRF
			// always) and EmailResend (IsAuthenticated).
			Path: "/api/v1/auth/password/reset/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireNever,
			Handler: a.passwordResetRequest,
		},
		Route{
			Path: "/api/v1/auth/password/reset/confirm/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireNever,
			Handler: a.passwordResetConfirm,
		},
		Route{
			Path: "/api/v1/auth/email/verify/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireNever,
			Handler: a.emailVerify,
		},
		Route{
			Path: "/api/v1/auth/email/resend/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireAlways,
			Handler: a.emailResend,
		},
		Route{
			Path: "/api/v1/auth/password/change/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFAlways, Session: SessionConsult, Require: RequireAlways,
			Handler: a.passwordChangeView,
		},
		Route{
			// ApiTokenListView (_SessionOnly): APIView subclass with
			// SessionAuthentication as its ONLY class + default
			// IsAuthenticated: anon -> 401 before any csrf check (CSRF only
			// for authenticated unsafe requests). Allow "GET, POST, HEAD,
			// OPTIONS" (get+post defined).
			Path: "/api/v1/auth/tokens/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.apiTokenList,
		},
		Route{
			// ApiTokenScopesView: Allow "GET, HEAD, OPTIONS".
			Path: "/api/v1/auth/tokens/scopes/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.apiTokenScopes,
		},
		Route{
			// ApiTokenDetailView: Allow "DELETE, OPTIONS". A non-uuid path
			// segment never matches (<uuid:pk> converter) and falls to
			// handler404's spaced dialect (probeable distinction).
			Path: "/api/v1/auth/tokens/{uuid}/", Methods: []string{"DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.apiTokenDelete,
		},
		Route{
			// designs SimpleRouter list/create (Allow "GET, POST, HEAD, OPTIONS"):
			// default SessionAuthentication consults the session even anonymously
			// (Vary: Cookie, probe0001 step06) + IsAuthenticated; CSRF for
			// authenticated unsafe requests only.
			Path: "/api/v1/designs/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsRoot,
		},
		Route{
			// retrieve/partial_update/destroy. fill/bulk-fill/fields/thumbnail/
			// bulk are separate detail/collection routes below.
			Path: "/api/v1/designs/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsDetail,
		},
		Route{
			// DesignViewSet.fill (detail action): authenticated, CSRF for
			// the unsafe method; 201 for a copy, 200 for an in-place edit.
			Path: "/api/v1/designs/{uuid}/fill/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designFill,
		},
		Route{
			// DesignViewSet.bulk_fill (url_path bulk-fill): always creates
			// copies and returns one result per row.
			Path: "/api/v1/designs/{uuid}/bulk-fill/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designBulkFill,
		},
		Route{
			// detail actions (DesignViewSet): fields is a plain GET, thumbnail
			// a multipart PUT; bulk is the collection-level action.
			Path: "/api/v1/designs/{uuid}/fields/", Methods: []string{"GET"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsFields,
		},
		Route{
			Path: "/api/v1/designs/{uuid}/thumbnail/", Methods: []string{"PUT"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsThumbnail,
		},
		Route{
			Path: "/api/v1/designs/bulk/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsBulk,
		},
		Route{
			Path: "/api/v1/designs/{uuid}/restore/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsRestore,
		},
		Route{
			Path: "/api/v1/designs/{uuid}/duplicate/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsDuplicate,
		},
		Route{
			// detail=False trash action (exact path beats the {uuid} template:
			// "trash" is not a uuid).
			Path: "/api/v1/designs/trash/", Methods: []string{"DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.designsTrashEmpty,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/versions/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.versionsRoot,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/versions/{version_pk}/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.versionsDetail,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/versions/{version_pk}/restore/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.versionsRestore,
		},
		Route{
			// FolderViewSet (list/create), pagination page_size=100.
			Path: "/api/v1/folders/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.foldersRoot,
		},
		Route{
			Path: "/api/v1/folders/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.foldersDetail,
		},
		Route{
			// Elements catalog: PUBLIC reads (authentication_classes=[] =>
			// no session consult, no Vary: Cookie) + cache_control 300.
			Path: "/api/v1/elements/packs/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.elementPacksList,
		},
		Route{
			Path: "/api/v1/elements/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.elementList,
		},
		Route{
			// Gallery public reads (same public-catalog pattern; no cache
			// header from the view -> middleware no-store).
			Path: "/api/v1/template-categories/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.templateCategoriesList,
		},
		Route{
			Path: "/api/v1/templates/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.templatesList,
		},
		Route{
			Path: "/api/v1/templates/{slug}/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.templateDetail,
		},
		Route{
			// TemplateUseView: default session auth + IsAuthenticated.
			Path: "/api/v1/templates/{slug}/use/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.templateUse,
		},
		Route{
			// BrandKitViewSet: list/create + detail CRUD (single-default
			// constraint, probe0005).
			Path: "/api/v1/brand-kits/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.brandList,
		},
		Route{
			Path: "/api/v1/brand-kits/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.brandDetail,
		},
		Route{
			// Fonts: PUBLIC catalogue (authentication_classes=[] ->
			// no session consult) + cache 300 from the view's finalize.
			Path: "/api/v1/fonts/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.fontList,
		},
		Route{
			// UserFontListCreate: list + multipart upload (session +
			// IsAuthenticated; CSRF for authenticated unsafe).
			Path: "/api/v1/fonts/mine/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.fontMine,
		},
		Route{
			Path: "/api/v1/fonts/mine/{uuid}/", Methods: []string{"DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.fontDelete,
		},
		Route{
			// AssetViewSet: list/create + detail CRUD + usage action
			// (probe0007: auth required everywhere; stock too).
			Path: "/api/v1/assets/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.assetsList,
		},
		Route{
			Path: "/api/v1/assets/usage/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.assetsUsage,
		},
		Route{
			Path: "/api/v1/assets/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.assetsDetail,
		},
		Route{
			// stock/*: default session auth (probe0007 steps01-04 -> 401);
			// POST imports need CSRF when authenticated.
			Path: "/api/v1/stock/providers/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockProvidersView,
		},
		Route{
			Path: "/api/v1/stock/photos/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockPhotosSearch,
		},
		Route{
			Path: "/api/v1/stock/photos/import/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockPhotoImport,
		},
		Route{
			// Stock videos (0.2.0): the same @api_view/APIView shapes as the
			// photo endpoints (IsAuthenticated, no CSRF on the GETs).
			Path: "/api/v1/stock/videos/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockVideosSearch,
		},
		Route{
			Path: "/api/v1/stock/videos/providers/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockVideosProviders,
		},
		Route{
			Path: "/api/v1/stock/videos/import/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockVideoImport,
		},
		Route{
			Path: "/api/v1/stock/audio/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockAudioSearch,
		},
		Route{
			Path: "/api/v1/stock/audio/info/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockAudioInfo,
		},
		Route{
			Path: "/api/v1/stock/audio/import/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockAudioImport,
		},
		Route{
			Path: "/api/v1/stock/audio/{sound_id}/preview/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.stockAudioPreview,
		},
		Route{
			// Sharing owner endpoints (owner-scoped design lookups).
			Path: "/api/v1/designs/{design_pk}/share-links/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.shareLinkList,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/share-links/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.shareLinkDetail,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/comments/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.commentsList,
		},
		Route{
			Path: "/api/v1/designs/{design_pk}/comments/{uuid}/", Methods: []string{"PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.commentDetail,
		},
		Route{
			// Public token views: shared finalize headers on EVERY response,
			// GET public / PATCH needs a session (RequireUnsafe), CSRF only
			// for authenticated unsafe requests (SessionAuthentication).
			Path: "/api/v1/shared/{token}/", Methods: []string{"GET", "PATCH"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireUnsafe, SharedHeaders: true,
			Handler: a.sharedRoot,
		},
		Route{
			Path: "/api/v1/shared/{token}/comments/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireUnsafe, SharedHeaders: true,
			Handler: a.sharedCommentsList,
		},
		Route{
			Path: "/api/v1/shared/{token}/comments/{uuid}/", Methods: []string{"PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireUnsafe, SharedHeaders: true,
			Handler: a.sharedCommentDetail,
		},
		Route{
			// Exports (apps/exports): pdf + image render + signed download.
			Path: "/api/v1/exports/pdf/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.pdfExport,
		},
		Route{
			Path: "/api/v1/designs/{uuid}/export/image/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.imageExport,
		},
		Route{
			// ImageDownloadView: authentication_classes=[] — the signed
			// token is the key (public route; no session consult).
			Path: "/api/v1/exports/image/{token}/", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.imageDownload,
		},
		Route{
			// AI (apps/ai): catalogue, contract, the user's own providers, and
			// the per-task run endpoint (slug LAST within /api/v1/ai/).
			Path: "/api/v1/ai/models/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleModelList,
		},
		Route{
			// ContractView: authentication_classes=[] — public byte-served file.
			Path: "/api/v1/ai/contract.yaml", Methods: []string{"GET"}, Style: AllowClass,
			Handler: a.HandleContract,
		},
		Route{
			Path: "/api/v1/ai/providers/", Methods: []string{"GET", "POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleProviderList,
		},
		Route{
			Path: "/api/v1/ai/providers/{uuid}/", Methods: []string{"GET", "PATCH", "DELETE"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleProviderDetail,
		},
		Route{
			Path: "/api/v1/ai/providers/{uuid}/load-models/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleProviderLoadModels,
		},
		Route{
			Path: "/api/v1/ai/providers/{uuid}/test/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleProviderTest,
		},
		Route{
			Path: "/api/v1/ai/providers/{uuid}/models/", Methods: []string{"PUT"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleProviderModels,
		},
		Route{
			Path: "/api/v1/ai/{task}/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.HandleTaskView,
		},
		Route{
			// Collab WebSocket (apps/collab, ASGI ws/designs/<uuid>/): the
			// origin validator refuses the handshake (HTTP403) inside the
			// handler;44xx codes are sent after accept (probe0012). Route
			// shape is the ORACLE's bare /ws/designs/<uuid>/ — the /api/v1
			// prefix is the API namespace, not the websocket route (ledger
			// routes-v2eb2557…: ws/designs/<uuid:design_id>/; probe0012).
			Path: "/ws/designs/{uuid}/", Methods: []string{"GET"}, Style: AllowClass,
			Session: SessionConsult,
			Handler: a.HandleCollabWS,
		},
		Route{
			// collab/stats: staff-only observability (IsAdminUser). A DRF
			// @api_view function view: Allow "GET, OPTIONS" (no HEAD — only
			// class-based views alias head=get; parity scenario 18 caught
			// the class-style "GET, HEAD, OPTIONS").
			Path: "/api/v1/collab/stats/", Methods: []string{"GET"}, Style: AllowAPIView,
			Session: SessionConsult, Require: RequireAlways,
			Handler: a.collabStats,
		},
		Route{
			// MCP server (apps/mcp, ASGI mcp_asgi; Streamable HTTP at
			// MCP_PATH): bearer-PAT JSON-RPC endpoint + /mcp/health. Not a
			// DRF view (own transport semantics: stateless json responses,
			// MCP_MAX_REQUEST_BYTES; Allow mirrors the JSON-RPC verb set for
			// the surfaced methods — probe0013).
			Path: "/mcp", Methods: []string{"POST"}, Style: AllowAPIView,
			Handler: a.mcpHandler,
		},
		Route{
			// MCP health (auth.py TokenAuthMiddleware: /mcp/health, no auth).
			Path: "/mcp/health", Methods: []string{"GET"}, Style: AllowAPIView, AllowNone: true,
			Handler: a.mcpHealth,
		},
		Route{
			// sitemap.xml: Django view at the ROOT (non-/api/, no Allow on
			// success, own cache header — probe0005 step05 bytes).
			Path: "/sitemap.xml", Methods: []string{"GET"}, Style: AllowClass, AllowNone: true,
			Handler: a.sitemap,
		},
		Route{
			// ImageOptimizeView: multipart PNG in, the converted file out
			// (IsAuthenticated; the optimize scope's throttle is pending with
			// the kernel rate-limit slice).
			Path: "/api/v1/exports/optimize/", Methods: []string{"POST"}, Style: AllowClass,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.exportsOptimize,
		},
		Route{
			// size_presets api_view: Allow "GET, OPTIONS", auth classes [] (no
			// session consult); cache_control public max-age=60 from the handler
			// (probe0004 step15).
			Path: "/api/v1/size-presets/", Methods: []string{"GET"}, Style: AllowAPIView,
			Handler: a.sizePresets,
		},
		Route{
			// my_sizes (@api_view ["GET", "PATCH"] + IsAuthenticated): the
			// signed-in person's pinned/recent/collapsed sizes (0.2.0).
			Path: "/api/v1/size-presets/mine/", Methods: []string{"GET", "PATCH"}, Style: AllowAPIView,
			Csrf: CSRFWhenAuthed, Session: SessionConsult, Require: RequireAlways,
			Handler: a.mySizes,
		},
	)
	a.Router = rt
	return a
}

// notPorted is the honest handler for a registered route whose view is not
// ported yet: reachable only behind guards that cannot pass (or for
// methods whose slice is pending — documented at the route). Remove when
// the owning family ports its view.
func (a *App) notPorted(view string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeDRF(w, http.StatusInternalServerError, "server_error",
			"route registered but not ported: "+view)
	}
}

// notFound ports handler404 for /api/ paths, plus CommonMiddleware's
// APPEND_SLASH redirect (GET/HEAD to the slashed form, 301, query kept).
func (a *App) notFound(w http.ResponseWriter, r *http.Request) {
	if isAdminPath(r.URL.Path) {
		a.adminDispatch(w, r)
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		!strings.HasSuffix(r.URL.Path, "/") {
		if _, slashed := a.Router.find(r.URL.Path + "/"); slashed {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeDjangoJSON(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	// Non-API 404: Django's page_not_found HTML (debug page under dev).
	http.NotFound(w, r)
}

// webappHandler lazily builds the embedded-frontend handler.
func (a *App) webappHandler(fallback http.Handler) *webappHandler {
	a.webappOnce.Do(func() {
		a.webapp = newWebappHandler(a.Settings.MediaRoot, fallback)
	})
	return a.webapp
}

// isAppPath reports the paths the Django oracle handles (API, admin, sockets,
// MCP, sitemap). Everything else is the SPA, /static or /media (M14).
func isAppPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/"),
		strings.HasPrefix(path, "/admin/"),
		strings.HasPrefix(path, "/ws/"),
		path == "/admin",
		path == "/mcp",
		path == "/mcp/health",
		path == "/sitemap.xml":
		return true
	}
	return false
}

// Handler builds the full stack: buffering (Django's assemble-then-send
// model) around the middleware chain around the router, with the embedded
// frontend serving everything the oracle does not own.
func (a *App) Handler() http.Handler {
	stack := buildStack(a.Router, a.Settings)
	// The webapp's fallback answers the paths it cannot serve (a missing media
	// file) through the same buffered stack the app paths use: middleware like
	// the CORS layer patches headers after the handler returns, which only
	// works before the response is flushed.
	buffered := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := newBufferedWriter(w)
		stack.ServeHTTP(buf, r)
		buf.flush()
	})
	webapp := a.webappHandler(buffered)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAppPath(r.URL.Path) {
			if strings.HasPrefix(r.URL.Path, "/ws/designs/") {
				// The WebSocket upgrade hijacks the connection; it must see
				// the raw writer (buffering breaks http.Hijacker — probe0012).
				stack.ServeHTTP(w, r)
				return
			}
			buf := newBufferedWriter(w)
			stack.ServeHTTP(buf, r)
			buf.flush()
			return
		}
		if a.Settings.DevProxyURL != "" {
			// Dev mode: the SPA is served by Vite; every non-API path is
			// proxied to it (frontend/vite.config.ts's server.proxy).
			proxyToDevServer(w, r, a.Settings.DevProxyURL)
			return
		}
		webapp.ServeHTTP(w, r)
	})
}

// timeNowUTC is the process clock (kept behind a helper so tests can pin it).
func timeNowUTC() time.Time { return time.Now().UTC() }

// ceil rounds up like math.Ceil for the throttled wait.
func ceil(f float64) float64 {
	i := int(f)
	if float64(i) < f {
		return float64(i + 1)
	}
	return float64(i)
}

// assetsDetail switches retrieve/patch/destroy.
func (a *App) assetsDetail(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.assetsRetrieve(w, r)
	case http.MethodPatch:
		a.assetsPatch(w, r)
	case http.MethodDelete:
		a.assetsDestroy(w, r)
	}
}

// sharedRoot switches the shared design GET/PATCH.
func (a *App) sharedRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		a.sharedDesignPatch(w, r)
		return
	}
	a.sharedDesignGet(w, r)
}
