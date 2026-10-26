# Admin inventory (D2, G27–G31)

Source of truth: `.upstream/current/backend/apps/*/admin.py`, the unfold
`UnfoldAdminSite` registry at the pin, and the 139 `admin/…` entries in
`ledger/routes-v2fb4d0297ec4.json`.

The Go admin is **route/status/state compatible, not markup compatible**: it
serves server-rendered HTML (no Unfold assets) and uses the same paths, HTTP
methods, redirects, form field names, DB writes and file side effects as the
oracle. Paragraph 8 of `PLAN.md` (D2) records the difference; `GAPS.md`
G27–G31 track the slices.

## Registered models (19 + `auth.Group`)

| # | app | model | table | PK | add | change | delete | list_per_page |
|---|---|---|---|---|---|---|---|---|
| 1 | admin | LogEntry | `django_admin_log` | int | no | no | no | – (history only) |
| 2 | auth | Group | `auth_group` | int | yes | yes | yes | 100 |
| 3 | accounts | User | `accounts_user` | uuid | yes | yes | yes | 50 |
| 4 | designs | Design | `designs_design` | uuid | no | yes | yes | 100 |
| 5 | designs | DesignVersion | `designs_designversion` | uuid | no | no | yes | 100 |
| 6 | designs | Folder | `designs_folder` | uuid | no | yes | yes | 100 |
| 7 | assets | Asset | `assets_asset` | uuid | no | yes | yes | 100 |
| 8 | gallery | TemplateCategory | `gallery_templatecategory` | uuid | yes | yes | yes | 100 |
| 9 | gallery | Template | `gallery_template` | uuid | yes | yes | yes | 40 |
| 10 | elements | ElementPack | `elements_elementpack` | uuid | yes | yes | yes | 100 |
| 11 | elements | Element | `elements_element` | uuid | yes | yes | yes | 60 |
| 12 | fonts | FontFamily | `fonts_fontfamily` | uuid | yes | yes | yes | 40 |
| 13 | fonts | FontVariant | `fonts_fontvariant` | uuid | inline | inline | inline | – |
| 14 | brand | BrandKit | `brand_brandkit` | uuid | no | yes | yes | 100 |
| 15 | sharing | ShareLink | `sharing_sharelink` | uuid | no | yes | yes | 100 |
| 16 | sharing | Comment | `sharing_comment` | uuid | no | no | yes | 100 |
| 17 | presets | SizePresetGroup | `presets_sizepresetgroup` | uuid | yes | yes | yes | 100 |
| 18 | presets | SizePreset | `presets_sizepreset` | uuid | yes | yes | yes | 100 |
| 19 | apitokens | ApiToken | `apitokens_apitoken` | uuid | no | yes | yes | 100 |
| 20 | ai | Provider | `ai_provider` (`owner IS NULL`) | uuid | yes | yes | yes | 100 |
| 21 | ai | UserProvider (proxy of Provider) | `ai_provider` (`owner IS NOT NULL`) | uuid | no | no | yes | 100 |
| 22 | ai | AICall | `ai_aicall` | uuid | no | no | yes | 100 |

`admin.LogEntry` is not registered as a model page; its rows back every
`…/history/` page. `auth.Permission` / `ContentType` / `Session` are
registered in the oracle but unfold's admin does not expose model pages for
them either (the route ledger contains no `admin/auth/permission/` entries).

## Site-level routes (11)

| route | view | method(s) | semantics |
|---|---|---|---|
| `admin/` | index | GET | app/model list; staff gate; `never_cache` |
| `admin/search/` | search | GET | `q` across registered `search_fields`; 200 page |
| `admin/login/` | login | GET, POST | `username`(email)+`password`+`next`; staff account required |
| `admin/logout/` | logout | POST | flush session, 302 `/admin/`; GET 405 |
| `admin/password_change/` | own password | GET, POST | `old_password`, `new_password1`, `new_password2` |
| `admin/password_change/done/` | done | GET | static page |
| `admin/autocomplete/` | autocomplete | GET | JSON `{"results":[{"id","text","selected"}],"pagination":{"more":false}}`; needs `app_label`, `model_name`, `field_name`, `term`, `page` |
| `admin/jsi18n/` | i18n catalog | GET | `text/javascript` |
| `admin/r/<content_type_id>/<object_id>/` | view on site | GET | 404: no registered model defines `get_absolute_url` |
| `admin/<app_label>/` | app index | GET | 404 for unknown app / no viewable models |
| `admin/<anything>` | catch-all | GET | APPEND_SLASH 301 else 404 |

## Per-model routes (6 × 20 = 120)

For every model above except `admin.LogEntry`:

```
admin/<app>/<model>/                          changelist   GET, POST (action / list_editable)
admin/<app>/<model>/add/                      add          GET, POST
admin/<app>/<model>/<object_id>/change/       change       GET, POST
admin/<app>/<model>/<object_id>/delete/       delete       GET, POST (`post=yes`)
admin/<app>/<model>/<object_id>/history/      history      GET
admin/<app>/<model>/<object_id>/              redirect     302 → change (or 404)
```

`<object_id>` is `<path:object_id>` upstream: the handler accepts the dashed
UUID (as rendered by upstream links) and the 32-hex storage form.

## Custom routes and detail actions (8)

| route | model | method(s) | semantics |
|---|---|---|---|
| `admin/accounts/user/<id>/password/` | User | GET, POST | admin password form (`password1`, `password2`) |
| `admin/gallery/template/<id>/validate/` | Template | GET | validate document, message, 302 change |
| `admin/gallery/template/<id>/duplicate/` | Template | GET | duplicate as draft, 302 to the copy |
| `admin/elements/elementpack/<id>/upload/` | ElementPack | GET, POST | bulk SVG upload form (`files`, `kind`) |
| `admin/elements/elementpack/<id>/upload-svgs/` | ElementPack | GET | 302 → `…/upload/` |
| `admin/ai/provider/env/<provider_id>/test/` | Provider | GET, POST | confirm page, then test; staff+change_provider |
| `admin/ai/provider/<id>/load-models/` | Provider | GET, POST | confirm page, then remote model list |
| `admin/ai/provider/<id>/test/` | Provider | GET, POST | confirm page, then test result page |

## Changelist contract

* `q` searches `search_fields` with case-insensitive contains (FK traversals
  join the target table).
* `list_filter` renders All/Yes/No for booleans, distinct values for choices
  and FKs, date drill-downs for dates; `?field__exact=…`, `?field__isnull=…`.
  `DesignAdmin` adds the custom `trashed=yes|no` filter.
* `o=1|-1|2|-2…` orders by `list_display` / declared ordering.
* `p=N` pagination, `list_per_page` per model.
* `list_editable` fields accept POST updates from the changelist.
* selected-action POST: `action`, `_selected_action` (repeated), `select_across`,
  `index`; actions run over the selected queryset and redirect back with a
  messages-cookie flash.

## Actions

| model | bulk actions |
|---|---|
| User | `mark_verified`, `activate_users`, `deactivate_users` (never the acting admin) |
| ApiToken | `revoke` (only non-revoked) |
| Design | `move_to_trash`, `restore_from_trash` |
| TemplateCategory | – |
| Template | `publish`, `unpublish`, `feature`, `unfeature`, `duplicate`, `validate_documents`; detail `validate`, `duplicate` |
| ElementPack | `publish`, `unpublish`; detail `upload-svgs` |
| Element | `publish`, `unpublish` |
| FontFamily | `publish`, `unpublish` |
| SizePresetGroup | `activate`, `deactivate` |
| SizePreset | `activate` (rolls back on a slug/alias clash), `deactivate` |
| Provider | detail `load-models`, `test` |
| DesignVersion, Folder, Asset, BrandKit, ShareLink, Comment, UserProvider, AICall, Group | – |

## Form/field rules worth naming

* User: `fieldsets`; add form is `email`, `display_name`, `usable_password`,
  `password1`, `password2`; change form exposes the encoded password value,
  groups + user_permissions horizontally; `avatar_preview`, `google_sub`,
  `last_login`, `created_at`, `updated_at` read-only. `is_staff`/`is_superuser`
  writes require the corresponding permission on `accounts.change_user`.
* Template: `document_file` (JSON upload, ≤16 MB) is required on add; rejects
  invalid UTF-8/JSON/documents; derives width/height/page_count/preset.
  `thumbnail` is re-encoded to WebP ≤720 px; replacing it deletes the old file
  after the save; deleting the row deletes the thumbnail. Tags are a
  comma-separated field normalized like `gallery.validators`.
* ElementPack: `thumbnail` is re-encoded to a 480 px WebP named
  `elements/packs/<slug>.webp`; `source_ref`/`synced_at` are read-only.
* Element: uploaded `svg` is sanitized by `processSVG`, stored as
  `elements/<pack slug>/<dashed id>.svg`, and refreshes width/height/colors/
  `is_recolorable`/`content_hash`; `key` is generated when empty; tags are
  lower-case/de-duplicated; `search_name`/`search_tags` stay in step; unknown
  `style`/`kind` values are rejected.
* FontFamily: variants arrive through the `variants-*` formset
  (`TOTAL_FORMS`, `INITIAL_FORMS`, `MIN_NUM_FORMS`, `MAX_NUM_FORMS`,
  `variants-<i>-weight|style|subset|file`); files are inspected by
  `internal/fontparse` (max 10 MB), stored under
  `fonts/system/<slug>/<dashed id>.<sniffed ext>`; owner is read-only.
* SizePreset: `tags`/`aliases` are comma-separated inputs mapped to JSON;
  aliases follow the preset-slug rule; `pixels`/`created_at`/`updated_at` are
  read-only; activating checks the active name clash and rolls back.
* Provider: `secret` never renders a value; empty keeps the stored secret,
  `clear_secret` removes it, a host change with an empty secret drops it;
  `tasks` is a multiple checkbox; `style_config` is JSON validated by the
  style; `base_url` is validated by `validate_base_url(operator=True)`;
  `auth_header` must not be a header Doory sets; inline `models-*` formset.
* ShareLink: `token`/`password_hash` are excluded from every page; only
  `is_active` is editable.
* Read-only pages (Asset, BrandKit, DesignVersion, UserProvider, AICall,
  ShareLink except `is_active`) still expose the change URL and history, but
  POSTs cannot alter read-only fields.

## Verification plan

* Probe `test/parity/probes/0018-admin/` records the oracle's statuses,
  redirects, `never_cache`/`Vary`, login/logout flow, action redirects,
  autocomplete JSON, history pages, and the 403 shapes.
* Scenario `test/parity/scenarios/admin/22-admin.toml` drives the same requests
  on both sides (`headers_only` for HTML), plus DB/file state checks.
* The 73 upstream `apps/*/tests/test_admin.py` behaviours are ported to Go
  integration tests (one test per behaviour, same fixtures where possible).
