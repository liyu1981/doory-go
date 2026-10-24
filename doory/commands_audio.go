// sync_audio (apps/assets/audiolib): install the bundled audio library into
// MEDIA_ROOT/audio/ from the files pinned in backend/data/audio_library.json
// (size and SHA-256 verified before a file is put in place) and write the
// catalogue the editor reads (audio/index.json).
//
// The pinned manifest lists no third-party packs; the pack path (archives from
// the manifest's hosts) is recorded as pending and answers a loud error, so
// nothing diverges silently for a manifest that does list packs.

package doory

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed data/audio_library.json
var audioLibraryJSON []byte

//go:embed data/audio
var audioFilesFS embed.FS

const (
	audioIndexName    = "index.json"
	audioMaxFileBytes = 25 * 1024 * 1024
	audioSchema       = 1
)

var audioHashedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]*-[0-9a-f]{12}\.(?:ogg|mp3|m4a|wav|weba)$`)

// audioTrack is manifest.TrackSpec (the fields the installer and the
// catalogue use).
type audioTrack struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Mood       string   `json:"mood"`
	Category   string   `json:"category"`
	Genre      string   `json:"genre"`
	Tags       []string `json:"tags"`
	BPM        int      `json:"bpm"`
	Key        string   `json:"key"`
	Loop       bool     `json:"loop"`
	Channels   int      `json:"channels"`
	DurationMs int      `json:"durationMs"`
	Lufs       *float64 `json:"lufs"`
	File       string   `json:"file"`
	Bytes      int64    `json:"bytes"`
	SHA256     string   `json:"sha256"`
	Peaks      []int    `json:"peaks"`
}

// ext is TrackSpec.ext.
func (t audioTrack) ext() string { return path.Ext(t.File)[1:] }

// storedName is TrackSpec.stored_name: stem-<sha[:12]>.ext.
func (t audioTrack) storedName() string {
	base := path.Base(t.File)
	ext := path.Ext(base)
	return strings.TrimSuffix(base, ext) + "-" + t.SHA256[:12] + ext
}

// relativePath is TrackSpec.relative_path (music/ or sfx/).
func (t audioTrack) relativePath() string {
	folder, _, _ := strings.Cut(t.File, "/")
	return folder + "/" + t.storedName()
}

// audioLicense is manifest.LicenseSpec.
type audioLicense struct {
	SPDX   string `json:"spdx"`
	URL    string `json:"url"`
	Holder string `json:"holder"`
	Note   string `json:"note,omitempty"`
}

// audioManifest is the embedded audio_library.json.
type audioManifest struct {
	Schema  int          `json:"schema"`
	Release string       `json:"release"`
	License audioLicense `json:"license"`
	Tracks  []audioTrack `json:"tracks"`
	Packs   []audioPack  `json:"packs"`
}

// audioPack is manifest.PackSpec: the pinned archive and its tracks.
type audioPack struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Homepage    string       `json:"homepage"`
	License     audioLicense `json:"license"`
	Attribution string       `json:"attribution"`
	Archive     audioArchive `json:"archive"`
	Tracks      []audioTrack `json:"tracks"`
}

// audioArchive is the pack's pinned zip.
type audioArchive struct {
	URL    string `json:"url"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
	Format string `json:"format"`
}

// spec turns the parsed pack into the installer's spec (archive name derived
// from the URL, like manifest.parse_pack).
func (p audioPack) spec() audioPackSpec {
	name := ""
	if parsed, err := urlParse(p.Archive.URL); err == nil {
		name = path.Base(parsed.Path)
	}
	if name == "" || name == "." || name == "/" {
		name = p.ID + ".zip"
	}
	return audioPackSpec{
		ID: p.ID, Name: p.Name, Description: p.Description, Homepage: p.Homepage,
		License: p.License, Attribution: p.Attribution, ArchiveURL: p.Archive.URL,
		ArchiveBytes: p.Archive.Bytes, ArchiveSHA256: p.Archive.SHA256, ArchiveName: name,
		Tracks: p.Tracks,
	}
}

func loadAudioManifest() (*audioManifest, error) {
	var manifest audioManifest
	if err := json.Unmarshal(audioLibraryJSON, &manifest); err != nil {
		return nil, fmt.Errorf("Cannot read audio_library.json: %v", err)
	}
	if manifest.Schema != audioSchema {
		return nil, fmt.Errorf("audio_library.json: schema must be %d.", audioSchema)
	}
	seen := map[string]bool{}
	for _, track := range manifest.Tracks {
		if seen[track.ID] {
			return nil, fmt.Errorf("audio_library.json: the track id %q appears twice.", track.ID)
		}
		seen[track.ID] = true
	}
	return &manifest, nil
}

// audioRoot is install.audio_root: MEDIA_ROOT/audio.
func (a *App) audioRoot() string { return filepath.Join(a.Settings.MediaRoot, "audio") }

// audioInstalled is install.is_installed: the file exists with the pinned size.
func audioInstalled(root string, track audioTrack) bool {
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(track.relativePath())))
	return err == nil && st.Mode().IsRegular() && st.Size() == track.Bytes
}

// audioIntact additionally verifies the pinned hash (install._intact).
func audioIntact(root string, track audioTrack) bool {
	target := filepath.Join(root, filepath.FromSlash(track.relativePath()))
	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() || st.Size() != track.Bytes {
		return false
	}
	sum, herr := sha256Of(target)
	return herr == nil && sum == track.SHA256
}

// audioReport is install.Report.
type audioReport struct {
	Installed    int
	UpToDate     int
	WouldInstall int
	Removed      []string
	Failed       []audioFailure
	IndexWritten bool
	Aborted      string
}

type audioFailure struct{ item, detail string }

// audioEntry is install.entry_json: one catalogue row, in field order.
type audioEntry struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Title      string          `json:"title"`
	Mood       string          `json:"mood,omitempty"`
	Category   string          `json:"category,omitempty"`
	Genre      string          `json:"genre,omitempty"`
	Tags       []string        `json:"tags"`
	BPM        int             `json:"bpm,omitempty"`
	Key        string          `json:"key,omitempty"`
	Loop       bool            `json:"loop"`
	Channels   int             `json:"channels"`
	DurationMs int             `json:"durationMs"`
	Lufs       json.RawMessage `json:"lufs,omitempty"`
	Bytes      int64           `json:"bytes"`
	Peaks      []int           `json:"peaks"`
	File       string          `json:"file"`
	License    string          `json:"license"`
	Source     string          `json:"source"`
}

// audioCatalogue is the index.json document, in field order.
type audioCatalogue struct {
	Schema  int          `json:"schema"`
	Release string       `json:"release"`
	License audioLicense `json:"license"`
	Tracks  []audioEntry `json:"tracks"`
	Packs   []any        `json:"packs"`
}

func newAudioEntry(track audioTrack, source, licenseID, relative string) audioEntry {
	entry := audioEntry{
		ID: track.ID, Kind: track.Kind, Title: track.Title, Tags: track.Tags, BPM: track.BPM,
		Key: track.Key, Loop: track.Loop, Channels: track.Channels, DurationMs: track.DurationMs,
		Lufs: audioLufsJSON(track.Lufs), Bytes: track.Bytes, Peaks: track.Peaks, File: relative,
		License: licenseID, Source: source,
	}
	if track.Kind == "music" {
		entry.Mood = track.Mood
	} else {
		entry.Category = track.Category
	}
	entry.Genre = track.Genre
	if entry.Tags == nil {
		entry.Tags = []string{}
	}
	if entry.Peaks == nil {
		entry.Peaks = []int{}
	}
	return entry
}

// audioLufsJSON renders the loudness the way Python's json.dumps renders a
// float (an integral float keeps its ".0": -16.0, not -16).
func audioLufsJSON(value *float64) json.RawMessage {
	if value == nil {
		return nil
	}
	text := pyFloatText(*value)
	return json.RawMessage(text)
}

// pyFloatText is Python's repr(float) for the values these catalogues carry.
func pyFloatText(value float64) string {
	if value == math.Trunc(value) && math.Abs(value) < 1e16 {
		return strconv.FormatFloat(value, 'f', 1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// audioOptions is install.Options.
type audioOptions struct {
	Force  bool
	DryRun bool
	Prune  bool
}

// syncAudioLibrary is install.sync for the bundled library.
func (a *App) syncAudioLibrary(manifest *audioManifest, sourceDir string, source audioPackSource,
	chosen map[string]bool, options audioOptions, log func(string),
) (*audioReport, error) {
	root := a.audioRoot()
	report := &audioReport{}
	if !options.DryRun {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, err
		}
	}
	keep := map[string]bool{}
	var entries []audioEntry
	for _, track := range manifest.Tracks {
		usable := a.installLibraryTrack(track, sourceDir, root, options, report)
		if last := len(report.Failed); last > 0 && report.Failed[last-1].item == track.ID {
			log(fmt.Sprintf("%s: %s", track.ID, report.Failed[last-1].detail))
		}
		if usable {
			entries = append(entries, newAudioEntry(track, "doory", manifest.License.SPDX, track.relativePath()))
			keep[track.relativePath()] = true
		}
	}
	// Packs that were installed earlier stay in the catalogue as long as the
	// manifest still lists them (installed = the files are in place).
	packSource := source
	previous := a.readAudioIndex()
	installed := map[string]map[string]string{}
	for _, pack := range manifest.Packs {
		if chosen[pack.ID] {
			if packSource == nil {
				report.Failed = append(report.Failed, audioFailure{pack.ID, "no source for the pack"})
				continue
			}
			relative, err := a.installAudioPack(pack, packSource, root, options, report, log)
			if err != nil {
				var unavailable *audioPackUnavailable
				if asAudioPackUnavailable(err, &unavailable) {
					report.Aborted = err.Error()
					break
				}
				report.Failed = append(report.Failed, audioFailure{pack.ID, err.Error()})
				continue
			}
			if relative != nil {
				installed[pack.ID] = relative
			}
			continue
		}
		kept := map[string]string{}
		if previous != nil {
			if prevTracks, ok := previous["tracks"].([]any); ok {
				for _, rawEntry := range prevTracks {
					entry, ok := rawEntry.(map[string]any)
					if !ok || entry["source"] != pack.ID {
						continue
					}
					if file, ok := entry["file"].(string); ok {
						if id, ok := entry["id"].(string); ok {
							kept[id] = file
						}
					}
				}
			}
		}
		ok := map[string]string{}
		for _, track := range pack.Tracks {
			relative, has := kept[track.ID]
			if !has {
				continue
			}
			st, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative)))
			if err == nil && st.Mode().IsRegular() && st.Size() == track.Bytes {
				ok[track.ID] = relative
			}
		}
		if len(ok) > 0 {
			installed[pack.ID] = ok
		}
	}
	var packEntries []audioEntry
	for _, pack := range manifest.Packs {
		for _, track := range pack.Tracks {
			relative, ok := installed[pack.ID][track.ID]
			if !ok {
				continue
			}
			if options.DryRun || fileExists(filepath.Join(root, filepath.FromSlash(relative))) {
				packEntries = append(packEntries, newAudioEntry(track, pack.ID, pack.License.SPDX, relative))
				keep[relative] = true
			}
		}
	}
	catalogue := audioCatalogue{
		Schema: audioSchema, Release: manifest.Release, License: manifest.License,
		Tracks: append(entries, packEntries...), Packs: []any{},
	}
	for _, pack := range manifest.Packs {
		if _, ok := installed[pack.ID]; !ok {
			continue
		}
		catalogue.Packs = append(catalogue.Packs, audioPackCatalogueEntry(pack))
	}
	_ = previous
	// Python writes the catalogue with separators (",", ":") plus a newline.
	encoded := encodeCompactJSON(catalogue)
	encoded = append(encoded, '\n')
	target := filepath.Join(root, audioIndexName)
	current, readErr := os.ReadFile(target)
	if len(entries) > 0 && (readErr != nil || string(current) != string(encoded)) && !options.DryRun {
		if err := writeFileAtomic(target, encoded); err != nil {
			return nil, err
		}
		report.IndexWritten = true
	}
	a.pruneAudio(root, keep, options, report)
	return report, nil
}

// installLibraryTrack is install._library_track.
func (a *App) installLibraryTrack(track audioTrack, sourceDir, root string, options audioOptions,
	report *audioReport,
) bool {
	if !options.Force && audioIntact(root, track) {
		report.UpToDate++
		return true
	}
	inPlace := audioIntact(root, track)
	source := filepath.Join(sourceDir, filepath.FromSlash(track.File))
	data, err := readAudioSource(source, track.File)
	if err != nil {
		report.Failed = append(report.Failed, audioFailure{track.ID,
			fmt.Sprintf("%s is not in %s", track.File, sourceDir)})
		return inPlace
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != track.Bytes || hex.EncodeToString(sum[:]) != track.SHA256 {
		report.Failed = append(report.Failed, audioFailure{track.ID,
			fmt.Sprintf("%s: the content does not match the manifest", track.File)})
		return inPlace
	}
	if problem := validateAudioContainer(data, track.ext()); problem != "" {
		report.Failed = append(report.Failed, audioFailure{track.ID,
			fmt.Sprintf("%s: not a valid audio file (%s)", track.File, problem)})
		return inPlace
	}
	if options.DryRun {
		report.WouldInstall++
		return true
	}
	target := filepath.Join(root, filepath.FromSlash(track.relativePath()))
	if err := writeFileAtomic(target, data); err != nil {
		report.Failed = append(report.Failed, audioFailure{track.ID, err.Error()})
		return inPlace
	}
	report.Installed++
	return true
}

// readAudioSource reads a library file from the data directory (--from-dir) or
// from the embedded copy (the binary's default source).
func readAudioSource(sourcePath, relative string) ([]byte, error) {
	if data, err := os.ReadFile(sourcePath); err == nil {
		return data, nil
	}
	return audioFilesFS.ReadFile(path.Join("data/audio", relative))
}

// validateAudioContainer is the content check sync_audio gets from
// assets.audio.parse_audio: the file must actually be the container its
// extension names. The deep metadata parse (duration/channels) is only needed
// by the upload pipeline and stays pending there.
func validateAudioContainer(data []byte, ext string) string {
	if len(data) < 12 {
		return "the file is too short"
	}
	switch ext {
	case "ogg":
		if string(data[:4]) != "OggS" {
			return "not an Ogg file"
		}
		if len(data) < 32 || !(containsBytes(data[:64], []byte("vorbis")) || containsBytes(data[:64], []byte("OpusHead"))) {
			return "no Vorbis/Opus identification header"
		}
	case "wav":
		if string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
			return "not a WAV file"
		}
	case "mp3":
		if !(string(data[:3]) == "ID3" || (data[0] == 0xFF && data[1]&0xE0 == 0xE0)) {
			return "not an MP3 file"
		}
	case "m4a":
		if !containsBytes(data[:64], []byte("ftyp")) {
			return "not an MP4 container"
		}
	case "weba":
		if !(data[0] == 0x1A && data[1] == 0x45 && data[2] == 0xDF && data[3] == 0xA3) {
			return "not a WebM/Matroska container"
		}
	default:
		return "unsupported audio extension"
	}
	return ""
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// pruneAudio is install._prune for the library folders (packs are pending).
func (a *App) pruneAudio(root string, keep map[string]bool, options audioOptions, report *audioReport) {
	for _, folder := range []string{"music", "sfx"} {
		dir := filepath.Join(root, folder)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			relative := folder + "/" + name
			if keep[relative] || !(audioHashedName.MatchString(name) || strings.HasSuffix(name, ".part")) {
				continue
			}
			report.Removed = append(report.Removed, relative)
			if !options.DryRun {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
	if options.Prune {
		packRoot := filepath.Join(root, "packs")
		if entries, err := os.ReadDir(packRoot); err == nil {
			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				names, _ := os.ReadDir(filepath.Join(packRoot, entry.Name()))
				for _, file := range names {
					if file.IsDir() {
						continue
					}
					relative := "packs/" + entry.Name() + "/" + file.Name()
					if keep[relative] {
						continue
					}
					report.Removed = append(report.Removed, relative)
					if !options.DryRun {
						_ = os.Remove(filepath.Join(packRoot, entry.Name(), file.Name()))
					}
				}
			}
		}
	}
}

// writeFileAtomic is install._atomic_write: a .part file renamed into place.
func writeFileAtomic(target string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".part")
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// readAudioIndex is install.read_index (the catalogue currently in place).
func (a *App) readAudioIndex() map[string]any {
	raw, err := os.ReadFile(filepath.Join(a.audioRoot(), audioIndexName))
	if err != nil {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	if _, ok := doc["tracks"].([]any); !ok {
		return nil
	}
	return doc
}

// runSyncAudio ports apps/assets/management/commands/sync_audio.py.
func runSyncAudio(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "from-dir", kind: "string", dest: "from_dir"},
		{name: "pack", kind: "list"},
		{name: "all-packs", kind: "bool", dest: "all_packs"},
		{name: "list", kind: "bool"},
		{name: "dry-run", kind: "bool", dest: "dry_run"},
		{name: "force", kind: "bool"},
		{name: "prune", kind: "bool"},
	})
	if err != nil {
		return err
	}
	manifest, merr := loadAudioManifest()
	if merr != nil {
		return commandErrorf("%v", merr)
	}
	if args.bool("list") {
		root := c.App.audioRoot()
		installed := 0
		music := 0
		for _, track := range manifest.Tracks {
			if audioInstalled(root, track) {
				installed++
			}
			if track.Kind == "music" {
				music++
			}
		}
		c.Printf("Library release %s (%s): %d music loops, %d sound effects, %d installed in %s\n",
			manifest.Release, manifest.License.SPDX, music, len(manifest.Tracks)-music, installed, root)
		if len(manifest.Packs) == 0 {
			c.Println("No packs are listed in the manifest.")
		}
		return nil
	}
	packs, perr := chooseAudioPacks(manifest, args.list("pack"), args.bool("all_packs"))
	if perr != nil {
		return perr
	}
	sourceDir := filepath.Join("data", "audio")
	if fromDir := args.str("from_dir"); fromDir != "" {
		st, serr := os.Stat(fromDir)
		if serr != nil || !st.IsDir() {
			return commandErrorf("%s: not a directory.", fromDir)
		}
		sourceDir = fromDir
	}
	dryRun := args.bool("dry_run")
	line := fmt.Sprintf("Syncing %d track(s) of release %s", len(manifest.Tracks), manifest.Release)
	if len(packs) > 0 {
		line += fmt.Sprintf(" and %d pack(s)", len(packs))
	}
	if dryRun {
		line += " - dry run, nothing is written"
	}
	c.Println(line + ".")
	var packSource audioPackSource
	if args.str("from_dir") != "" {
		packSource = audioDirectoryPackSource{dir: sourceDir}
	} else {
		packSource = newAudioHTTPPackSource()
	}
	chosenSet := map[string]bool{}
	for _, pack := range packs {
		chosenSet[pack.ID] = true
	}
	report, serr := c.App.syncAudioLibrary(manifest, sourceDir, packSource, chosenSet, audioOptions{
		Force: args.bool("force"), DryRun: dryRun, Prune: args.bool("prune"),
	}, func(line string) { c.Errorf("%s\n", line) })
	if serr != nil {
		return commandErrorf("%v", serr)
	}
	summary := fmt.Sprintf("Done: %d installed, %d already up to date", report.Installed, report.UpToDate)
	if dryRun {
		summary += fmt.Sprintf(", %d would be installed", report.WouldInstall)
	}
	summary += fmt.Sprintf(", %d removed, %d failed", len(report.Removed), len(report.Failed))
	if report.IndexWritten {
		summary += "; the catalogue was updated"
	}
	c.Println(summary + ".")
	for _, failure := range report.Failed {
		c.Errorf("  %s: %s\n", failure.item, failure.detail)
	}
	if report.Aborted != "" {
		return commandErrorf("%s. Check your network or proxy settings (HTTPS_PROXY), or download the pack "+
			"archives on a machine with internet access and install them with --from-dir DIR (see THIRD_PARTY.md).",
			report.Aborted)
	}
	if len(report.Failed) > 0 {
		items := make([]string, 0, len(report.Failed))
		for i, failure := range report.Failed {
			if i == 8 {
				break
			}
			items = append(items, failure.item)
		}
		return commandErrorf("%d item(s) failed: %s. Nothing that failed is listed in the catalogue; "+
			"run the command again to retry.", len(report.Failed), strings.Join(items, ", "))
	}
	return nil
}

// chooseAudioPacks ports Command._choose_packs.
func chooseAudioPacks(manifest *audioManifest, ids []string, everything bool) ([]audioPack, error) {
	if everything {
		return manifest.Packs, nil
	}
	if len(ids) == 0 {
		return nil, nil
	}
	known := map[string]audioPack{}
	var names []string
	for _, pack := range manifest.Packs {
		known[pack.ID] = pack
		names = append(names, pack.ID)
	}
	var unknown []string
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		list := strings.Join(names, ", ")
		if list == "" {
			list = "none"
		}
		return nil, commandErrorf("Unknown pack id(s): %s. Packs in the manifest: %s.",
			strings.Join(unknown, ", "), list)
	}
	var chosen []audioPack
	for _, pack := range manifest.Packs {
		if containsString(ids, pack.ID) {
			chosen = append(chosen, pack)
		}
	}
	return chosen, nil
}
