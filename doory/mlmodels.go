// Ports backend/apps/core/mlmodels/manifest.py
// Ports backend/apps/core/mlmodels/install.py
// Ports backend/apps/core/mlmodels/availability.py
package doory

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed data/ml_models.json
var manifestJSON []byte

// Manifest mirrors data/ml_models.json (schema + models).
type Manifest struct {
	Models []ManifestModel `json:"models"`
}

type ManifestModel struct {
	ID       string        `json:"id"`
	Task     string        `json:"task"`
	Name     string        `json:"name"`
	Priority int           `json:"priority"`
	File     string        `json:"file"`
	Bytes    int64         `json:"bytes"`
	SHA256   string        `json:"sha256"`
	Input    ManifestInput `json:"input"`
	Output   string        `json:"output"`
}

type ManifestInput struct {
	Name    string    `json:"name"`
	Size    int       `json:"size"`
	Mean    []float64 `json:"mean"`
	Std     []float64 `json:"std"`
	Rescale string    `json:"rescale"`
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
