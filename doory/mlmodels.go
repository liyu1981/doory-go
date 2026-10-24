// Ports backend/apps/core/mlmodels/manifest.py
// Ports backend/apps/core/mlmodels/install.py
// Ports backend/apps/core/mlmodels/availability.py
package doory

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed data/ml_models.json
var manifestJSON []byte

// Manifest mirrors data/ml_models.json (schema + models).
type Manifest struct {
	Schema int             `json:"schema"`
	Models []ManifestModel `json:"models"`
}

// ManifestModel is manifest.ModelSpec (the fields sync_models and /config/ use).
type ManifestModel struct {
	ID          string          `json:"id"`
	Task        string          `json:"task"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Priority    int             `json:"priority"`
	Default     bool            `json:"default"`
	File        string          `json:"file"`
	Bytes       int64           `json:"bytes"`
	SHA256      string          `json:"sha256"`
	URLs        []string        `json:"urls"`
	Homepage    string          `json:"homepage"`
	License     ManifestLicense `json:"license"`
	Input       ManifestInput   `json:"input"`
	Output      string          `json:"output"`
}

// ManifestLicense is manifest.LicenseSpec.
type ManifestLicense struct {
	SPDX     string `json:"spdx"`
	URL      string `json:"url"`
	Holder   string `json:"holder"`
	Evidence string `json:"evidence"`
}

type ManifestInput struct {
	Name    string    `json:"name"`
	Size    int       `json:"size"`
	Mean    []float64 `json:"mean"`
	Std     []float64 `json:"std"`
	Rescale string    `json:"rescale"`
}

// notice ports ModelSpec.notice: the text written next to the model file so
// the licence travels with the weights.
func (m ManifestModel) notice() string {
	return fmt.Sprintf("%s\n%s\n\nThe weights in %s are distributed under the %s licence (%s).\n%s\n\n"+
		"Doory downloaded this exact file (SHA-256 %s) from the sources listed in backend/data/ml_models.json "+
		"and did not modify it.\n",
		m.Name, m.Homepage, m.storedName(), m.License.SPDX, m.License.URL, m.License.Holder, m.SHA256)
}

// defaults ports Manifest.defaults.
func (m *Manifest) defaults() []ManifestModel {
	out := []ManifestModel{}
	for _, model := range m.Models {
		if model.Default {
			out = append(out, model)
		}
	}
	return out
}

// selectModels ports Manifest.select: the named ids in manifest order, every
// model (--all), or the defaults.
func (m *Manifest) selectModels(ids []string, everything bool) ([]ManifestModel, error) {
	if everything {
		return append([]ManifestModel{}, m.Models...), nil
	}
	if len(ids) == 0 {
		return m.defaults(), nil
	}
	known := map[string]bool{}
	for _, model := range m.Models {
		known[model.ID] = true
	}
	var unknown []string
	for _, id := range ids {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		names := make([]string, len(m.Models))
		for i, model := range m.Models {
			names[i] = model.ID
		}
		return nil, fmt.Errorf("Unknown model id(s): %s. Known models: %s.",
			strings.Join(unknown, ", "), strings.Join(names, ", "))
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	out := []ManifestModel{}
	for _, model := range m.Models {
		if wanted[model.ID] {
			out = append(out, model)
		}
	}
	return out, nil
}

// loadManifest parses the embedded manifest. Parse failure mirrors
// ManifestError -> no models (availability falls back to nil).
func loadManifest() (*Manifest, bool) {
	var m Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return nil, false
	}
	return &m, true
}

// storedName ports ModelSpec.stored_name: stem-<sha[:12]>.suffix.
func (m ManifestModel) storedName() string {
	stem, ext, found := strings.Cut(m.File, ".")
	if !found {
		return m.File
	}
	sha := m.SHA256
	if len(sha) > 12 {
		sha = sha[:12]
	}
	return stem + "-" + sha + "." + ext
}

func (m ManifestModel) relativePath() string {
	return "models/" + m.ID + "/" + m.storedName()
}

func (m ManifestModel) installed(modelsRoot string) bool {
	// install.py is_installed: file exists with the pinned size.
	path := filepath.Join(modelsRoot, m.ID, m.storedName())
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() == m.Bytes
}

// mlModelDescription ports mlmodels describe().
type mlModelDescription struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	URL    string        `json:"url"`
	Bytes  int64         `json:"bytes"`
	SHA256 string        `json:"sha256"`
	Input  ManifestInput `json:"input"`
	Output string        `json:"output"`
}

func describe(m ManifestModel) *mlModelDescription {
	return &mlModelDescription{
		ID: m.ID, Name: m.Name, URL: mediaURL + m.relativePath(),
		Bytes: m.Bytes, SHA256: m.SHA256, Input: m.Input, Output: m.Output,
	}
}

// bestInstalled ports mlmodels best_installed(): highest (priority, id)
// among installed models for the task, pinned by BACKGROUND_REMOVAL_MODEL
// when that env names one.
func bestInstalled(s *Settings, task string) *ManifestModel {
	manifest, ok := loadManifest()
	if !ok {
		return nil // ManifestError -> None
	}
	modelsRoot := filepath.Join(s.MediaRoot, "models")
	var candidates []ManifestModel
	for _, m := range manifest.Models {
		if m.Task != task {
			continue
		}
		if s.BackgroundRemovalPin != "" && m.ID != s.BackgroundRemovalPin {
			continue
		}
		if m.installed(modelsRoot) {
			candidates = append(candidates, m)
		}
	}
	// max by (priority, id)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Priority != candidates[j].Priority {
			return candidates[i].Priority > candidates[j].Priority
		}
		return candidates[i].ID > candidates[j].ID
	})
	if len(candidates) == 0 {
		return nil
	}
	out := candidates[0]
	return &out
}

// backgroundRemovalModel ports mlmodels availability.background_removal_model():
// None when nothing usable is installed.
func backgroundRemovalModel(s *Settings) *mlModelDescription {
	spec := bestInstalled(s, "background-removal")
	if spec == nil {
		return nil
	}
	return describe(*spec)
}
