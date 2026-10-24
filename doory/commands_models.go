// sync_models: install the machine-learning models the editor runs in the
// browser into MEDIA_ROOT/models/, from the exact files pinned in
// backend/data/ml_models.json (size and SHA-256 verified before a file is put
// in place). Idempotent and resumable.
//
// Sources: a local directory (--from-dir, the offline path the cli-driver
// exercises) or HTTPS from the manifest's URLs with resumable Range requests,
// bounded retries and a host allow-list.

package doory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	modelNoticeName  = "NOTICE.txt"
	modelConnectSecs = 10
	modelReadSecs    = 60
	modelAttempts    = 3
	modelMaxRedirect = 5
	modelChunk       = 256 * 1024
)

// allowedModelHosts is manifest.ALLOWED_HOSTS / ALLOWED_HOST_SUFFIXES.
var allowedModelHosts = []string{"github.com", "huggingface.co"}
var allowedModelHostSuffixes = []string{".githubusercontent.com", ".huggingface.co", ".hf.co"}

func modelHostAllowed(host string) bool {
	if host == "" {
		return false
	}
	for _, h := range allowedModelHosts {
		if host == h {
			return true
		}
	}
	for _, suffix := range allowedModelHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// modelInstallResult is install.InstallResult.
type modelInstallResult struct {
	ID     string
	Status string // installed | up-to-date | would-install | failed
	Detail string
	// unreachable: the source cannot be reached at all (stop instead of trying
	// every remaining model).
	Unreachable bool
}

// modelInstallReport is install.InstallReport.
type modelInstallReport struct {
	Results []modelInstallResult
	Aborted string
}

func (r *modelInstallReport) failed() []modelInstallResult {
	out := []modelInstallResult{}
	for _, res := range r.Results {
		if res.Status == "failed" {
			out = append(out, res)
		}
	}
	return out
}

// sourceError / sourceUnavailable / integrityError mirror sources.py's
// exception family.
type sourceError struct{ message string }

func (e *sourceError) Error() string { return e.message }

type sourceUnavailable struct{ sourceError }

type integrityError struct{ sourceError }

// modelSource is sources.ModelSource: put a model's bytes at destination.
type modelSource interface {
	fetch(spec ManifestModel, destination string, onProgress func(done, total int64),
		verify func(path string) error) error
}

// directorySource ports sources.DirectorySource (`--from-dir`).
type directorySource struct{ dir string }

func (s *directorySource) fetch(spec ManifestModel, destination string,
	onProgress func(done, total int64), verify func(path string) error,
) error {
	candidates := []string{
		filepath.Join(s.dir, spec.File),
		filepath.Join(s.dir, spec.ID, spec.File),
	}
	for _, candidate := range candidates {
		st, err := os.Stat(candidate)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if st.Size() > spec.Bytes {
			return &sourceError{message: fmt.Sprintf("%s is larger than the %d bytes the manifest pins.",
				candidate, spec.Bytes)}
		}
		if err := copyFile(candidate, destination); err != nil {
			return &sourceError{message: err.Error()}
		}
		if onProgress != nil {
			onProgress(spec.Bytes, spec.Bytes)
		}
		if verify != nil {
			return verify(destination)
		}
		return nil
	}
	return &sourceError{message: fmt.Sprintf("Neither %s nor %s/%s was found in %s.",
		spec.File, spec.ID, spec.File, s.dir)}
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// httpModelSource ports sources.HttpModelSource: resumable download, bounded
// retries, redirects checked against the host allow-list, hard size cap.
type httpModelSource struct {
	client *http.Client
	sleep  func(time.Duration)
}

func newHTTPModelSource() *httpModelSource {
	return &httpModelSource{
		client: &http.Client{Timeout: 0}, // per-request timeouts below
		sleep:  time.Sleep,
	}
}

func (s *httpModelSource) fetch(spec ManifestModel, destination string,
	onProgress func(done, total int64), verify func(path string) error,
) error {
	var errors []string
	for _, rawURL := range spec.URLs {
		err := s.fetchURL(rawURL, spec.Bytes, destination, onProgress)
		if err == nil && verify != nil {
			err = verify(destination)
		}
		var unavailable *sourceUnavailable
		if asSourceUnavailable(err, &unavailable) {
			return err
		}
		if err != nil {
			errors = append(errors, err.Error())
			_ = os.Remove(destination) // a different mirror must not continue this one's bytes
			continue
		}
		return nil
	}
	if len(errors) == 0 {
		return &sourceError{message: fmt.Sprintf("No source for %s.", spec.ID)}
	}
	return &sourceError{message: strings.Join(errors, "; ")}
}

func asSourceUnavailable(err error, target **sourceUnavailable) bool {
	if err == nil {
		return false
	}
	u, ok := err.(*sourceUnavailable)
	if ok {
		*target = u
	}
	return ok
}

func (s *httpModelSource) fetchURL(rawURL string, total int64, destination string,
	onProgress func(done, total int64),
) error {
	var last error
	var hint time.Duration
	for attempt := 0; attempt < modelAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * time.Second
			if hint > wait {
				wait = hint
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			s.sleep(wait)
			hint = 0
		}
		err := s.download(rawURL, total, destination, onProgress)
		if err == nil {
			return nil
		}
		var retry *modelRetry
		if asModelRetry(err, &retry) {
			last, hint = err, retry.delay
			continue
		}
		last = err
	}
	host := hostOf(rawURL)
	if isUnreachable(last) {
		return &sourceUnavailable{sourceError{message: fmt.Sprintf("Cannot reach %s: %v", host, last)}}
	}
	if last != nil {
		return &sourceError{message: fmt.Sprintf("%s: %v", host, last)}
	}
	return &sourceError{message: fmt.Sprintf("Download from %s failed", host)}
}

type modelRetry struct {
	message string
	delay   time.Duration
}

func (e *modelRetry) Error() string { return e.message }

func asModelRetry(err error, target **modelRetry) bool {
	r, ok := err.(*modelRetry)
	if ok {
		*target = r
	}
	return ok
}

func isUnreachable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Cannot reach") || strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") || strings.Contains(msg, "timeout")
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

// download does one HTTP attempt (with redirects followed by hand).
func (s *httpModelSource) download(rawURL string, total int64, destination string,
	onProgress func(done, total int64),
) error {
	offset := int64(0)
	if st, err := os.Stat(destination); err == nil {
		offset = st.Size()
		if offset >= total { // more than expected can only be garbage: start over
			_ = os.Remove(destination)
			offset = 0
		}
	}
	resp, finalURL, err := s.open(rawURL, offset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 429 || resp.StatusCode == 500 || resp.StatusCode == 502 ||
		resp.StatusCode == 503 || resp.StatusCode == 504:
		delay := time.Duration(0)
		if secs, perr := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); perr == nil {
			delay = time.Duration(secs) * time.Second
		}
		return &modelRetry{message: fmt.Sprintf("HTTP %d", resp.StatusCode), delay: delay}
	case resp.StatusCode == 416:
		_ = os.Remove(destination)
		return &modelRetry{message: "the partial download no longer matches"}
	case resp.StatusCode != 200 && resp.StatusCode != 206:
		return &sourceError{message: fmt.Sprintf("HTTP %d from %s", resp.StatusCode, hostOf(finalURL))}
	}
	resumed := resp.StatusCode == 206 && offset > 0 && rangeStart(resp) == offset
	if !resumed {
		offset = 0
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if resumed {
		flags = os.O_WRONLY | os.O_CREATE
	}
	out, oerr := os.OpenFile(destination, flags, 0o644)
	if oerr != nil {
		return &sourceError{message: oerr.Error()}
	}
	defer out.Close()
	if resumed {
		if _, serr := out.Seek(offset, io.SeekStart); serr != nil {
			return &sourceError{message: serr.Error()}
		}
	}
	written := offset
	buf := make([]byte, modelChunk)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > total {
				return &sourceError{message: fmt.Sprintf(
					"The file is larger than the %d bytes the manifest pins.", total)}
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return &sourceError{message: werr.Error()}
			}
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return &sourceError{message: rerr.Error()}
		}
	}
}

// open follows redirects by hand so every hop is checked against the manifest's
// host allow-list, keeping the Range header.
func (s *httpModelSource) open(rawURL string, offset int64) (*http.Response, string, error) {
	current := rawURL
	for hop := 0; hop <= modelMaxRedirect; hop++ {
		parsed, err := url.Parse(current)
		if err != nil || parsed.Scheme != "https" || !modelHostAllowed(parsed.Hostname()) {
			return nil, current, &sourceError{message: fmt.Sprintf("Refusing to download from %s.",
				parsed.Hostname())}
		}
		req, rerr := http.NewRequest(http.MethodGet, current, nil)
		if rerr != nil {
			return nil, current, &sourceError{message: rerr.Error()}
		}
		req.Header.Set("User-Agent", "Doory-model-sync/1.0")
		if offset > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		}
		resp, derr := s.client.Do(req)
		if derr != nil {
			return nil, current, &sourceUnavailable{sourceError{message: fmt.Sprintf("Cannot reach %s",
				parsed.Hostname())}}
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if location == "" {
				return nil, current, &sourceError{message: fmt.Sprintf(
					"Redirect without a target from %s.", parsed.Hostname())}
			}
			next, jerr := url.Parse(location)
			if jerr != nil {
				return nil, current, &sourceError{message: jerr.Error()}
			}
			current = resp.Request.URL.ResolveReference(next).String()
			continue
		}
		return resp, current, nil
	}
	return nil, current, &sourceError{message: "Too many redirects."}
}

// rangeStart reads the first byte of a 206 response (Content-Range).
func rangeStart(resp *http.Response) int64 {
	header := resp.Header.Get("Content-Range")
	unit, rest, _ := strings.Cut(header, " ")
	start, _, _ := strings.Cut(rest, "-")
	if unit != "bytes" {
		return -1
	}
	n, err := strconv.ParseInt(start, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// sha256Of streams a file's digest in 1 MiB chunks.
func sha256Of(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	digest := sha256.New()
	buf := make([]byte, 1024*1024)
	for {
		n, rerr := handle.Read(buf)
		if n > 0 {
			digest.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// verifyModelFile ports install.verify_file.
func verifyModelFile(spec ManifestModel, path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return &integrityError{sourceError{message: fmt.Sprintf("%s: %v", spec.File, err)}}
	}
	if st.Size() != spec.Bytes {
		return &integrityError{sourceError{message: fmt.Sprintf("%s: %d bytes, the manifest pins %d",
			spec.File, st.Size(), spec.Bytes)}}
	}
	sum, serr := sha256Of(path)
	if serr != nil {
		return &integrityError{sourceError{message: fmt.Sprintf("%s: %v", spec.File, serr)}}
	}
	if sum != spec.SHA256 {
		return &integrityError{sourceError{message: fmt.Sprintf(
			"%s: the content does not match the pinned SHA-256", spec.File)}}
	}
	return nil
}

func (m ManifestModel) modelPath(root string) string {
	return filepath.Join(root, m.ID, m.storedName())
}

// installModels ports install.install_models.
func (a *App) installModels(specs []ManifestModel, source modelSource, force, dryRun bool,
	log func(string),
) *modelInstallReport {
	report := &modelInstallReport{}
	root := filepath.Join(a.Settings.MediaRoot, "models")
	for _, spec := range specs {
		result := a.installOneModel(spec, source, root, force, dryRun, log)
		report.Results = append(report.Results, result)
		line := fmt.Sprintf("%s: %s", spec.ID, result.Status)
		if result.Detail != "" {
			line += fmt.Sprintf(" (%s)", result.Detail)
		}
		log(line)
		if result.Unreachable {
			report.Aborted = result.Detail
			break
		}
	}
	return report
}

// installOneModel ports install._install_one.
func (a *App) installOneModel(spec ManifestModel, source modelSource, root string, force, dryRun bool,
	log func(string),
) modelInstallResult {
	final := spec.modelPath(root)
	if !force && verifyModelFile(spec, final) == nil && fileExists(final) {
		a.writeModelNotice(spec, filepath.Dir(final), dryRun)
		return modelInstallResult{ID: spec.ID, Status: "up-to-date"}
	}
	if dryRun {
		return modelInstallResult{ID: spec.ID, Status: "would-install",
			Detail: fmt.Sprintf("%.1f MB", float64(spec.Bytes)/1e6)}
	}
	part := final + ".part"
	milestones := &modelMilestones{spec: spec, log: log}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return modelInstallResult{ID: spec.ID, Status: "failed",
			Detail: fmt.Sprintf("cannot write %s: %v", filepath.Dir(final), err)}
	}
	if force {
		_ = os.Remove(part)
	}
	ferr := source.fetch(spec, part, milestones.call, func(path string) error {
		return verifyModelFile(spec, path)
	})
	if ferr == nil {
		ferr = verifyModelFile(spec, part)
	}
	if ferr != nil {
		var integrity *integrityError
		if asIntegrityError(ferr, &integrity) {
			_ = os.Remove(part)
			return modelInstallResult{ID: spec.ID, Status: "failed", Detail: ferr.Error()}
		}
		var unavailable *sourceUnavailable
		if asSourceUnavailable(ferr, &unavailable) {
			return modelInstallResult{ID: spec.ID, Status: "failed", Detail: ferr.Error(), Unreachable: true}
		}
		return modelInstallResult{ID: spec.ID, Status: "failed", Detail: ferr.Error()}
	}
	if err := os.Chmod(part, 0o644); err != nil {
		return modelInstallResult{ID: spec.ID, Status: "failed",
			Detail: fmt.Sprintf("cannot write %s: %v", filepath.Dir(final), err)}
	}
	if err := os.Rename(part, final); err != nil {
		return modelInstallResult{ID: spec.ID, Status: "failed",
			Detail: fmt.Sprintf("cannot write %s: %v", filepath.Dir(final), err)}
	}
	a.writeModelNotice(spec, filepath.Dir(final), false)
	removeStaleModelFiles(spec, final)
	return modelInstallResult{ID: spec.ID, Status: "installed", Detail: fmt.Sprintf("%.1f MB",
		float64(spec.Bytes)/1e6)}
}

func asIntegrityError(err error, target **integrityError) bool {
	i, ok := err.(*integrityError)
	if ok {
		*target = i
	}
	return ok
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// writeModelNotice ports install._write_notice.
func (a *App) writeModelNotice(spec ManifestModel, folder string, dryRun bool) {
	if dryRun {
		return
	}
	notice := filepath.Join(folder, modelNoticeName)
	if existing, err := os.ReadFile(notice); err == nil && string(existing) == spec.notice() {
		return
	}
	_ = os.WriteFile(notice, []byte(spec.notice()), 0o644)
}

// removeStaleModelFiles ports install._remove_stale: an earlier release of the
// same model (and abandoned partials).
func removeStaleModelFiles(spec ManifestModel, keep string) {
	stem := strings.TrimSuffix(spec.File, path.Ext(spec.File))
	entries, err := os.ReadDir(filepath.Dir(keep))
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Join(filepath.Dir(keep), name) == keep || !strings.HasPrefix(name, stem+"-") {
			continue
		}
		if hashedModelName.MatchString(name) {
			_ = os.Remove(filepath.Join(filepath.Dir(keep), name))
		}
	}
}

// hashedModelName is install._HASHED.
var hashedModelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*-[0-9a-f]{12}\.onnx(?:\.part)?$`)

// modelMilestones logs every 20 % so a large download shows life.
type modelMilestones struct {
	spec  ManifestModel
	log   func(string)
	shown int
}

func (m *modelMilestones) call(done, total int64) {
	if total <= 0 {
		total = 1
	}
	step := done * 5 / total
	if step > int64(m.shown) {
		m.shown = int(step)
		m.log(fmt.Sprintf("%s: %d%% (%.0f of %.0f MB)", m.spec.ID, m.shown*20,
			float64(done)/1e6, float64(total)/1e6))
	}
}

// runSyncModels ports apps/core/management/commands/sync_models.py.
func runSyncModels(c *CLI, argv []string) error {
	args, err := parseArgs(argv, []argSpec{
		{name: "model", kind: "list"},
		{name: "all", kind: "bool"},
		{name: "list", kind: "bool"},
		{name: "dry-run", kind: "bool", dest: "dry_run"},
		{name: "force", kind: "bool"},
		{name: "from-dir", kind: "string", dest: "from_dir"},
	})
	if err != nil {
		return err
	}
	manifest, ok := loadManifest()
	if !ok {
		return commandErrorf("The model manifest is not valid.")
	}
	specs, serr := manifest.selectModels(args.list("model"), args.bool("all"))
	if serr != nil {
		return commandErrorf("%v", serr)
	}
	if args.bool("list") {
		root := filepath.Join(c.App.Settings.MediaRoot, "models")
		width := 0
		for _, spec := range manifest.Models {
			if len(spec.ID) > width {
				width = len(spec.ID)
			}
		}
		for _, spec := range manifest.Models {
			state := "not installed"
			if verifyModelFile(spec, spec.modelPath(root)) == nil {
				state = "installed"
			}
			tags := spec.License.SPDX
			if spec.Default {
				tags = "default, " + tags
			}
			c.Printf("%-*s  %-18s %7.1f MB  %-14s [%s] (%s)\n", width, spec.ID, spec.Task,
				float64(spec.Bytes)/1e6, spec.File, state, tags)
			c.Printf("%-*s  %s\n", width, "", spec.modelPath(root))
		}
		return nil
	}
	var source modelSource
	if fromDir := args.str("from_dir"); fromDir != "" {
		st, serr := os.Stat(fromDir)
		if serr != nil || !st.IsDir() {
			return commandErrorf("%s: not a directory.", fromDir)
		}
		source = &directorySource{dir: fromDir}
	} else {
		source = newHTTPModelSource()
	}
	dryRun := args.bool("dry_run")
	line := fmt.Sprintf("Syncing %d model(s)", len(specs))
	if dryRun {
		line += " - dry run, nothing is written"
	}
	c.Println(line + ".")
	report := c.App.installModels(specs, source, args.bool("force"), dryRun, c.Println)
	installed, current, pending := 0, 0, 0
	for _, res := range report.Results {
		switch res.Status {
		case "installed":
			installed++
		case "up-to-date":
			current++
		case "would-install":
			pending++
		}
	}
	failed := len(report.failed())
	summary := fmt.Sprintf("Done: %d of %d model(s) ok; %d installed, %d already up to date",
		len(report.Results)-failed, len(report.Results), installed, current)
	if dryRun {
		summary += fmt.Sprintf(", %d would be installed", pending)
	}
	summary += fmt.Sprintf(", %d failed.", failed)
	c.Println(summary)
	if report.Aborted != "" {
		return commandErrorf("%s. Check your network or proxy settings (HTTPS_PROXY), or download the "+
			"files on a machine with internet access and install them with --from-dir DIR (see THIRD_PARTY.md).",
			report.Aborted)
	}
	if failed > 0 {
		ids := make([]string, 0, failed)
		for _, res := range report.failed() {
			ids = append(ids, res.ID)
		}
		return commandErrorf("%d model(s) failed: %s. Run the command again to retry.",
			failed, strings.Join(ids, ", "))
	}
	return nil
}
