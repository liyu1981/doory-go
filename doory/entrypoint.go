// The container entrypoint and health probe (backend/docker-entrypoint.sh and
// backend/healthcheck.py), specialized for the single binary:
//
//   - the Django-only preparation steps have no Go counterpart: the cache is
//     in-process (no createcachetable) and static files are embedded (no
//     collectstatic, D7);
//   - the optional catalog bootstraps are the same env switches, each run only
//     when the command exists and the switch is on, and never fatal (the
//     upstream script starts the server anyway).

package doory

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WaitForDatabase ports docker-entrypoint.sh wait_for_database: 30 attempts,
// two seconds apart, then a fatal message.
func WaitForDatabase(db *sql.DB, out io.Writer) error {
	var last error
	for attempt := 1; attempt <= 30; attempt++ {
		if err := db.Ping(); err == nil {
			return nil
		} else {
			last = err
		}
		if out != nil {
			fmt.Fprintf(out, "Waiting for the database (%d/30): %v\n", attempt, last)
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("The database did not become available in time.")
}

// prepareEnv is the subset of the entrypoint's environment the bootstraps use.
type prepareEnv struct {
	SyncFonts       bool
	SyncElements    bool
	SyncModels      bool
	SyncAudio       bool
	SeedTemplates   bool
	SizePresetsFile string
	SizePresetsMode string
	SyncFontsArgs   []string
	SyncElementsArg []string
	SyncModelsArgs  []string
	SyncAudioArgs   []string
}

func prepareEnvFromOS() prepareEnv {
	env := prepareEnv{
		SyncFonts:       envBool("DOORY_SYNC_FONTS", false),
		SyncElements:    envBool("DOORY_SYNC_ELEMENTS", false),
		SyncModels:      envBool("DOORY_SYNC_MODELS", false),
		SyncAudio:       envBool("DOORY_SYNC_AUDIO", false),
		SeedTemplates:   envBool("DOORY_SEED_TEMPLATES", false),
		SizePresetsFile: envStr("DOORY_SIZE_PRESETS_FILE", ""),
		SizePresetsMode: envStr("DOORY_SIZE_PRESETS_MODE", "extend"),
		SyncFontsArgs:   splitArgs(envStr("DOORY_SYNC_FONTS_ARGS", "--popular 100")),
		SyncElementsArg: splitArgs(envStr("DOORY_SYNC_ELEMENTS_ARGS", "")),
		SyncModelsArgs:  splitArgs(envStr("DOORY_SYNC_MODELS_ARGS", "")),
		SyncAudioArgs:   splitArgs(envStr("DOORY_SYNC_AUDIO_ARGS", "")),
	}
	return env
}

func splitArgs(raw string) []string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// PrepareApp ports the entrypoint's "gunicorn" branch: migrate, then the
// optional catalog bootstraps. Each bootstrap failure is reported and the
// server still starts.
func PrepareApp(app *App, out io.Writer) error {
	if err := app.migrateEmbedded(); err != nil {
		return err
	}
	env := prepareEnvFromOS()
	run := func(name string, args []string) bool {
		if !HasCommand(name) {
			return true
		}
		return RunCLI(app, append([]string{name}, args...), out, out) == 0
	}
	if env.SyncFonts {
		marker := filepath.Join(app.Settings.MediaRoot, "fonts", ".synced")
		if _, err := os.Stat(marker); err != nil {
			fmt.Fprintln(out, "Downloading the font catalog (first start)...")
			if run("sync_fonts", env.SyncFontsArgs) {
				_ = os.MkdirAll(filepath.Dir(marker), 0o755)
				_ = os.WriteFile(marker, nil, 0o644)
			} else {
				fmt.Fprintln(os.Stderr, "Font sync failed; the server starts anyway. Run 'doory sync_fonts' again later.")
			}
		}
	}
	if env.SyncElements && HasCommand("sync_element_packs") {
		fmt.Fprintln(out, "Installing the element catalog (shapes, graphics, icon, emoji and illustration packs)...")
		if !run("seed_elements", nil) {
			fmt.Fprintln(os.Stderr, "seed_elements failed; the server starts anyway.")
		}
		if !run("sync_element_packs", env.SyncElementsArg) {
			fmt.Fprintln(os.Stderr, "Element sync failed; the server starts anyway. Run 'doory sync_element_packs' again later.")
		}
	}
	if env.SyncModels && HasCommand("sync_models") {
		fmt.Fprintln(out, "Installing the background-removal model...")
		if !run("sync_models", env.SyncModelsArgs) {
			fmt.Fprintln(os.Stderr, "Model sync failed; the server starts anyway. Run 'doory sync_models' again later.")
		}
	}
	if env.SyncAudio && HasCommand("sync_audio") {
		fmt.Fprintln(out, "Installing the audio library (music loops and sound effects)...")
		if !run("sync_audio", env.SyncAudioArgs) {
			fmt.Fprintln(os.Stderr, "Audio sync failed; the server starts anyway. Run 'doory sync_audio' again later.")
		}
	}
	if env.SeedTemplates && HasCommand("seed_templates") {
		fmt.Fprintln(out, "Adding the starter templates (existing ones are left alone)...")
		if !run("seed_templates", nil) {
			fmt.Fprintln(os.Stderr, "Template seeding failed; the server starts anyway. Run 'doory seed_templates' again later.")
		}
	}
	if env.SizePresetsFile != "" && HasCommand("import_size_presets") {
		mode := env.SizePresetsMode
		if mode == "" {
			mode = "extend"
		}
		fmt.Fprintf(out, "Importing size presets from %s (%s)...\n", env.SizePresetsFile, mode)
		if !run("import_size_presets", []string{env.SizePresetsFile, "--mode", mode}) {
			fmt.Fprintln(os.Stderr,
				"Size preset import failed; the server starts anyway with the presets it had. Fix the file and restart.")
		}
	}
	return nil
}

// migrateEmbedded applies the embedded migrations with the app's database.
func (a *App) migrateEmbedded() error {
	return Migrate(a.DB, a.Settings.DatabaseURL)
}

// Healthcheck ports healthcheck.py: probe 127.0.0.1:HEALTH_PORT + HEALTH_PATH
// with a Host header Django's ALLOWED_HOSTS accepts; exit 0 only on 200.
func Healthcheck() int {
	hosts := strings.Split(envStr("DJANGO_ALLOWED_HOSTS", "localhost"), ",")
	host := "localhost"
	for _, raw := range hosts {
		candidate := strings.TrimLeft(strings.TrimSpace(raw), ".")
		if candidate != "" && candidate != "*" {
			host = candidate
			break
		}
	}
	port := envStr("HEALTH_PORT", "8000")
	path := envStr("HEALTH_PATH", "/api/v1/health/")
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+port+path, nil)
	if err != nil {
		return 1
	}
	request.Host = host
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return 0
	}
	return 1
}
