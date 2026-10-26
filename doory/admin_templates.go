// Admin page models + the embedded template set. The markup is intentionally
// not django-unfold's (D2); it renders the same information from the same
// state so the HTML checks in the ported tests keep their meaning.
package doory

import (
	"embed"
	"fmt"
	"html/template"
	"strings"
)

//go:embed admin/templates/*.html
var adminTemplateFS embed.FS

// page models

type adminLink struct {
	Label string
	URL   string
}

type adminAppBlock struct {
	Name   string
	Label  string
	Models []adminLink
}

type adminIndexPage struct {
	adminBase
	Apps []adminAppBlock
}

type adminPagination struct {
	Show    bool
	Page    int
	Pages   int
	PrevURL string
	NextURL string
	Start   int
	End     int
	Total   int
}

// adminEnvRow is one environment provider in the ai.provider changelist panel.
type adminEnvRow struct {
	ID      string
	Name    string
	Style   string
	Tasks   string
	Models  string
	Quota   string
	TestURL string
}

type adminChangelistPage struct {
	adminBase
	Model          *adminModel
	PageTitle      string
	ObjectName     string
	Columns        []adminColumn
	Rows           []adminListRow
	Filters        []adminFilter
	SearchValue    string
	ShowSearch     bool
	Actions        []adminAction
	ShowActions    bool
	ShowListForm   bool
	EditableFields []adminField
	AddURL         string
	ShowAdd        bool
	ResultCount    int
	FullCount      int
	Pagination     adminPagination
	Preserved      string
	DateLinks      []adminLink
	// EnvProviders/EnvErrors are the ai.provider panel for AI_PROVIDER_n_*
	// providers (apps/ai/admin.py changelist_view).
	EnvProviders []adminEnvRow
	EnvErrors    []string
	// ShowResultCount mirrors Django's "N result(s)" line.
	ShowResultCount bool
}

func (p *adminChangelistPage) ChangeURL(id string) string {
	return "/admin/" + p.Model.AppLabel + "/" + p.Model.Model + "/" + adminURLID(p.Model, id) + "/change/"
}

func (p *adminChangelistPage) EditableValue(row adminListRow, name string) string {
	return row.Editable[name]
}

type adminChangePage struct {
	adminBase
	Model       *adminModel
	Object      *adminObject
	IsAdd       bool
	Fieldsets   []adminFieldset
	Inlines     []adminInline
	Errors      adminErrors
	ShowDelete  bool
	ShowHistory bool
	DeleteURL   string
	HistoryURL  string
	PasswordURL string
	SubmitLabel string
	// ShowSaveAsNew mirrors ModelAdmin.save_as.
	ShowSaveAsNew bool
	InlineErrors  adminErrors
	// Multi carries checkbox/multi-select values (groups, permissions, tasks).
	Multi map[string][]string
	// Cookies is the titles of horizontal-filter widgets (unused markup).
	CookieNames []string
}

func (p *adminChangePage) Value(name string) string {
	if p.Object == nil {
		return ""
	}
	if v, ok := p.Object.Values[name]; ok {
		return v
	}
	return ""
}

func (i adminInline) TotalForms() int { return len(i.Rows) + len(i.ExtraRows) }

func (i adminInline) DisplayValue(id, name string) template.HTML {
	for _, row := range i.Rows {
		if row.ID == id {
			return row.Display[name]
		}
	}
	return ""
}

func (i adminInline) Value(id, name string) string {
	for _, row := range i.Rows {
		if row.ID == id {
			return row.Values[name]
		}
	}
	return ""
}

// RenderExtraInput draws one empty inline row's input.
func (i adminInline) RenderExtraInput(name string, index int) template.HTML {
	field := name
	inputName := fmt.Sprintf("%s-%d-%s", i.Prefix, index, name)
	kind := adminText
	var choices []adminFilterChoice
	for _, c := range i.Columns {
		if c.Name == name {
			kind, choices = c.Kind, c.Choices
		}
	}
	var b strings.Builder
	b.WriteString(`<input type="hidden" name="` + fmt.Sprintf("%s-%d-id", i.Prefix, index) + `" value="">`)
	switch kind {
	case adminSelect:
		b.WriteString(`<select name="` + inputName + `">`)
		for _, c := range choices {
			b.WriteString(`<option value="` + djangoEscape(c.Value) + `">` + djangoEscape(c.Label) + `</option>`)
		}
		b.WriteString(`</select>`)
	case adminFile:
		b.WriteString(`<input type="file" name="` + inputName + `">`)
	case adminBool:
		b.WriteString(`<input type="checkbox" name="` + inputName + `">`)
	default:
		b.WriteString(`<input type="text" name="` + inputName + `" value="">`)
	}
	_ = field
	return template.HTML(b.String())
}

func (p *adminChangePage) Checked(name string) bool {
	switch strings.ToLower(p.Value(name)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func (p *adminChangePage) FieldErrors(name string) []string {
	return p.Errors[name]
}

func (p *adminChangePage) DisplayValue(f adminField) template.HTML {
	if p.Object != nil {
		if v, ok := p.Object.Display[f.Name]; ok {
			return v
		}
		if f.Display != nil {
			return f.Display(p.Object)
		}
		if v, ok := p.Object.Values[f.Name]; ok {
			return template.HTML(djangoEscape(v))
		}
	}
	return ""
}

type adminDeletePage struct {
	adminBase
	Model     *adminModel
	Object    *adminObject
	DeleteURL string
	BackURL   string
}

type adminDeleteSelectedPage struct {
	adminBase
	Model      *adminModel
	Objects    []*adminObject
	ObjectName string
	BackURL    string
}

type adminHistoryEntry struct {
	Time    string
	User    string
	Action  string
	Message string
}

type adminHistoryPage struct {
	adminBase
	Model   *adminModel
	Object  *adminObject
	Entries []adminHistoryEntry
	BackURL string
}

type adminPasswordChangePage struct {
	adminBase
	ObjectID string
	Target   *adminObject
	Errors   adminErrors
	Self     bool
}

func (p *adminPasswordChangePage) FieldErrors(name string) []string { return p.Errors[name] }

type adminLoginPage struct {
	adminBase
	Next     string
	Error    string
	Username string
	AppPath  string
}

type adminSearchPage struct {
	adminBase
	Query   string
	Results []adminSearchHit
}

type adminSearchHit struct {
	Model string
	Label string
	Title string
	URL   string
}

type adminUploadPage struct {
	adminBase
	Model   *adminModel
	Pack    *adminObject
	Error   string
	Action  string
	Cancel  string
	MaxFile int
}

type adminConfirmPage struct {
	adminBase
	Model       *adminModel
	Provider    *adminObject
	Message     string
	Confirm     string
	ActionURL   string
	BackURL     string
	Detail      string
	TestOK      bool
	TestImage   string
	TestMessage string
}

// djangoEscape is Python's html.escape (quotes become &quot;/&#x27;), which
// is what Django's conditional_escape produces inside format_html.
func djangoEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#x27;",
	)
	return r.Replace(s)
}

// loadAdminTemplates parses the layout around every page template.
func loadAdminTemplates() map[string]*template.Template {
	layoutBytes, err := adminTemplateFS.ReadFile("admin/templates/layout.html")
	if err != nil {
		panic(err)
	}
	entries, err := adminTemplateFS.ReadDir("admin/templates")
	if err != nil {
		panic(err)
	}
	out := map[string]*template.Template{}
	for _, entry := range entries {
		name := entry.Name()
		if name == "layout.html" || !strings.HasSuffix(name, ".html") {
			continue
		}
		pageBytes, err := adminTemplateFS.ReadFile("admin/templates/" + name)
		if err != nil {
			continue
		}
		tmpl, err := template.New("layout").Parse(string(layoutBytes))
		if err != nil {
			panic(fmt.Sprintf("admin template %s: %v", name, err))
		}
		if _, err := tmpl.Parse(string(pageBytes)); err != nil {
			panic(fmt.Sprintf("admin template %s: %v", name, err))
		}
		out[strings.TrimSuffix(name, ".html")] = tmpl
	}
	return out
}
