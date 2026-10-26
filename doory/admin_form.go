// Admin form parsing: the submitted values and uploads behind every add /
// change / delete POST. Django parses multipart once (request.POST /
// request.FILES); the hooks receive the same view of the request.
package doory

import (
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// adminUpload is one uploaded file (Django's InMemoryUploadedFile/Temporary).
type adminUpload struct {
	Filename    string
	ContentType string
	Data        []byte
}

// maxAdminUploadBytes bounds a single admin upload (Django's 2.5 MB memory
// threshold spills to disk upstream; the Go admin keeps it in memory).
const maxAdminUploadBytes = 64 << 20

// adminForm is the parsed request body.
type adminForm struct {
	Values url.Values
	Files  map[string][]adminUpload
	// Raw is the original multipart form (file handles already consumed
	// above; kept nil for urlencoded posts).
	Raw *multipart.Form
}

// newAdminForm parses urlencoded or multipart bodies.
func newAdminForm(r *http.Request) (*adminForm, error) {
	f := &adminForm{Values: url.Values{}, Files: map[string][]adminUpload{}}
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return f, err
		}
		f.Raw = r.MultipartForm
		if r.MultipartForm != nil {
			for name, values := range r.MultipartForm.Value {
				f.Values[name] = append([]string(nil), values...)
			}
			for name, headers := range r.MultipartForm.File {
				for _, header := range headers {
					upload, err := readAdminUpload(header)
					if err != nil {
						return f, err
					}
					f.Files[name] = append(f.Files[name], upload)
				}
			}
		}
		return f, nil
	}
	if err := r.ParseForm(); err != nil {
		return f, err
	}
	for name, values := range r.PostForm {
		f.Values[name] = append([]string(nil), values...)
	}
	return f, nil
}

func readAdminUpload(header *multipart.FileHeader) (adminUpload, error) {
	handle, err := header.Open()
	if err != nil {
		return adminUpload{}, err
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, maxAdminUploadBytes+1))
	if err != nil {
		return adminUpload{}, err
	}
	if len(data) > maxAdminUploadBytes {
		data = data[:maxAdminUploadBytes]
	}
	return adminUpload{
		Filename:    header.Filename,
		ContentType: header.Header.Get("Content-Type"),
		Data:        data,
	}, nil
}

// Value returns the first submitted value.
func (f *adminForm) Value(name string) string {
	if f == nil || f.Values == nil {
		return ""
	}
	return f.Values.Get(name)
}

// Has reports whether the field was present (checkboxes submit only when on).
func (f *adminForm) Has(name string) bool {
	if f == nil || f.Values == nil {
		return false
	}
	_, ok := f.Values[name]
	return ok
}

// All returns every submitted value for a repeated field.
func (f *adminForm) All(name string) []string {
	if f == nil || f.Values == nil {
		return nil
	}
	return f.Values[name]
}

// Bool is the cleaned checkbox value.
func (f *adminForm) Bool(name string) bool {
	return f.Has(name) && !adminFalseText(f.Value(name))
}

// Int parses a submitted integer (0 when absent/invalid).
func (f *adminForm) Int(name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(f.Value(name)))
	return n
}

// Float parses a submitted number.
func (f *adminForm) Float(name string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(f.Value(name)), 64)
	return v, err == nil
}

// File returns the first upload for a field (nil when none).
func (f *adminForm) File(name string) *adminUpload {
	if f == nil || len(f.Files[name]) == 0 {
		return nil
	}
	upload := f.Files[name][0]
	if upload.Filename == "" && len(upload.Data) == 0 {
		return nil
	}
	return &upload
}

// Uploads returns every upload for a repeated field.
func (f *adminForm) Uploads(name string) []adminUpload {
	if f == nil {
		return nil
	}
	return f.Files[name]
}

// adminFalseText mirrors BooleanField.to_python's false vocabulary.
func adminFalseText(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "false", "0", "off", "no":
		return true
	}
	return false
}

// Multi returns the object's repeated values (groups, permissions, tasks).
func (o *adminObject) Multi(name string) []string {
	if o == nil || o.Raw == nil {
		return nil
	}
	switch v := o.Raw[name].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// formFields is every editable field, in fieldset order with leftovers last.
func (m *adminModel) formFields() []adminField {
	seen := map[string]bool{}
	var out []adminField
	for _, set := range m.Fieldsets {
		for _, field := range set.Fields {
			if !seen[field.Name] {
				seen[field.Name] = true
				out = append(out, field)
			}
		}
	}
	for _, field := range m.FormFields {
		if !seen[field.Name] {
			seen[field.Name] = true
			out = append(out, field)
		}
	}
	return out
}

// adminSubmittedObject keeps the POSTed values on the re-rendered form.
func adminSubmittedObject(m *adminModel, form *adminForm) *adminObject {
	obj := &adminObject{
		Values:  map[string]string{},
		Display: map[string]template.HTML{},
		Raw:     map[string]any{},
	}
	for _, field := range m.formFields() {
		switch field.Kind {
		case adminCheckboxMulti, adminMultiSelect:
			obj.Raw[field.Name] = form.All(field.Name)
		case adminFile, adminPassword:
			// uploads/passwords never round-trip
		case adminBool:
			if form.Has(field.Name) {
				obj.Values[field.Name] = "1"
			} else {
				obj.Values[field.Name] = ""
			}
		default:
			obj.Values[field.Name] = form.Value(field.Name)
		}
	}
	// Inline management fields are not in formFields; the model hook reads
	// the form directly.
	return obj
}

// SelectedValues resolves the checked values for checkbox/multi widgets.
func (p *adminChangePage) SelectedValues(name string) map[string]bool {
	out := map[string]bool{}
	if p.Object != nil {
		for _, v := range p.Object.Multi(name) {
			out[v] = true
		}
	}
	for _, v := range p.Multi[name] {
		out[v] = true
	}
	return out
}

// adminResolveFieldsets copies the fieldsets and fills DB-backed choices.
func (a *App) adminResolveFieldsets(m *adminModel, sets []adminFieldset) []adminFieldset {
	out := make([]adminFieldset, len(sets))
	for i, set := range sets {
		out[i] = set
		out[i].Fields = append([]adminField(nil), set.Fields...)
		for j := range out[i].Fields {
			field := &out[i].Fields[j]
			if len(field.Choices) == 0 && m.Choices != nil {
				if fn := m.Choices[field.Name]; fn != nil {
					field.Choices = fn(a)
				}
			}
		}
	}
	return out
}
