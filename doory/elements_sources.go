// Where sync_element_packs gets artwork from (apps/elements/sources.py and the
// generated-artwork source of generated.py).
//
// The pipeline only asks a source for the files of a pack, so the seam is the
// same as upstream: an npm release as one tarball (verified against the pinned
// integrity hash BEFORE anything is read from it), a list of individual files
// (each verified against its SHA-256), the same content already on disk
// (--from-dir) or artwork generated in this repository (the pinned output is
// embedded — D14).

package doory

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed data/elements_generated
var generatedArtworkFS embed.FS

const (
	packArchiveMaxBytes = 120 * 1024 * 1024
	packMemberMaxBytes  = 64 * 1024 * 1024
	packTotalMaxBytes   = 400 * 1024 * 1024
	packMaxMembers      = 100_000
	packURLFileMaxBytes = 5 * 1024 * 1024
	packUserAgent       = "Doory-element-sync/1.0"
	packAttempts        = 3
)

// packSourceError / packSourceUnavailable / packIntegrityError mirror
// sources.py's exception family.
type packSourceError struct{ message string }

func (e *packSourceError) Error() string { return e.message }

type packSourceUnavailable struct{ packSourceError }

type packIntegrityError struct{ packSourceError }

func packSourceErrorf(format string, args ...any) error {
	return &packSourceError{message: fmt.Sprintf(format, args...)}
}

func asPackUnavailable(err error, target **packSourceUnavailable) bool {
	u, ok := err.(*packSourceUnavailable)
	if ok {
		*target = u
	}
	return ok
}

func asPackIntegrity(err error, target **packIntegrityError) bool {
	i, ok := err.(*packIntegrityError)
	if ok {
		*target = i
	}
	return ok
}

// packSource is sources.PackSource: the files of a spec matching the patterns.
type packSource interface {
	fetch(spec elementSource, patterns []string) (map[string][]byte, error)
}

// labelOf names a source for messages (spec.label()).
func labelOf(spec elementSource) string { return spec.label() }

// globRegex is sources.glob_regex: `*` (not across /), `**` (across /), `?`.
func globRegex(pattern string) *regexp.Regexp {
	var out strings.Builder
	for i := 0; i < len(pattern); {
		switch {
		case strings.HasPrefix(pattern[i:], "**/"):
			out.WriteString("(?:.*/)?")
			i += 3
		case strings.HasPrefix(pattern[i:], "**"):
			out.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			out.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			out.WriteString("[^/]")
			i++
		default:
			out.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	// Python compiles the pattern and uses re.match (anchored at the start);
	// `*` never crosses "/" and `**` does.
	return regexp.MustCompile(`\A(?s)` + out.String() + `\z`)
}

// matcher is sources.matcher.
func matcher(patterns []string) func(string) bool {
	compiled := make([]*regexp.Regexp, len(patterns))
	for i, pattern := range patterns {
		compiled[i] = globRegex(pattern)
	}
	return func(rel string) bool {
		for _, rx := range compiled {
			if rx.MatchString(rel) {
				return true
			}
		}
		return false
	}
}

// archivePath is sources.archive_path: drop the top-level folder, refuse
// unsafe names.
func archivePath(name string) (string, bool) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	cleaned := path.Clean(normalized)
	parts := strings.Split(cleaned, "/")
	if strings.HasPrefix(normalized, "/") || containsString(parts, "..") || len(parts) < 2 ||
		containsString(parts, "") {
		return "", false
	}
	return strings.Join(parts[1:], "/"), true
}

// verifyPackIntegrity is sources.verify_integrity (npm sha512 values).
func verifyPackIntegrity(data []byte, integrity, what string) error {
	algorithm, expected, ok := strings.Cut(integrity, "-")
	if !ok || algorithm != "sha512" {
		return &packIntegrityError{packSourceError{message: fmt.Sprintf(
			"%s: unsupported integrity algorithm %q", what, algorithm)}}
	}
	sum := sha512.Sum512(data)
	actual := base64.StdEncoding.EncodeToString(sum[:])
	if actual != expected {
		return &packIntegrityError{packSourceError{message: fmt.Sprintf(
			"%s: the content does not match the pinned integrity hash", what)}}
	}
	return nil
}

// readTarball is sources.read_tarball: the matching regular files of a gzipped
// tarball, paths relative to its top folder, nothing extracted to disk.
func readTarball(data []byte, patterns []string) (map[string][]byte, error) {
	wanted := matcher(patterns)
	found := map[string][]byte{}
	total := 0
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, packSourceErrorf("The archive could not be read: %v", err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	count := 0
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, packSourceErrorf("The archive could not be read: %v", err)
		}
		count++
		if count > packMaxMembers {
			return nil, packSourceErrorf("The archive holds too many files.")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		rel, ok := archivePath(header.Name)
		if !ok || !wanted(rel) {
			continue
		}
		if header.Size > packMemberMaxBytes {
			return nil, packSourceErrorf("%s is larger than %d MB.", rel, packMemberMaxBytes/(1<<20))
		}
		total += int(header.Size)
		if total > packTotalMaxBytes {
			return nil, packSourceErrorf("The requested files add up to more than the size limit.")
		}
		content, rerr := io.ReadAll(archive)
		if rerr != nil {
			return nil, packSourceErrorf("The archive could not be read: %v", rerr)
		}
		found[rel] = content
	}
	return found, nil
}

// readPackDirectory is sources.read_directory (symbolic links never followed).
func readPackDirectory(root string, patterns []string) (map[string][]byte, error) {
	wanted := matcher(patterns)
	found := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(root, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(root, current)
		if rerr != nil {
			return nil
		}
		slash := filepath.ToSlash(rel)
		if !wanted(slash) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Size() > packMemberMaxBytes {
			return packSourceErrorf("%s is larger than %d MB.", slash, packMemberMaxBytes/(1<<20))
		}
		total += int(info.Size())
		if total > packTotalMaxBytes {
			return packSourceErrorf("The requested files add up to more than the size limit.")
		}
		content, cerr := os.ReadFile(current)
		if cerr != nil {
			return nil
		}
		found[slash] = content
		return nil
	})
	return found, err
}

// noPackSource is sources.NoSource: a run that only involves generated packs.
type noPackSource struct{}

func (noPackSource) fetch(spec elementSource, patterns []string) (map[string][]byte, error) {
	return nil, packSourceErrorf("%s needs a download source; run sync_element_packs.", spec.label())
}

// directoryPackSource is sources.DirectorySource (--from-dir).
type directoryPackSource struct{ dir string }

func (s directoryPackSource) fetch(spec elementSource, patterns []string) (map[string][]byte, error) {
	switch src := spec.(type) {
	case npmSource:
		tarball := filepath.Join(s.dir, src.tarballName())
		if st, err := os.Stat(tarball); err == nil && st.Mode().IsRegular() {
			if st.Size() > packArchiveMaxBytes {
				return nil, packSourceErrorf("%s is too large.", src.tarballName())
			}
			data, rerr := os.ReadFile(tarball)
			if rerr != nil {
				return nil, packSourceErrorf("%v", rerr)
			}
			if err := verifyPackIntegrity(data, src.Integrity, src.tarballName()); err != nil {
				return nil, err
			}
			return readTarball(data, patterns)
		}
		folder := filepath.Join(s.dir, strings.ReplaceAll(strings.TrimPrefix(src.Package, "@"), "/", "-"))
		if st, err := os.Stat(folder); err == nil && st.IsDir() {
			return readPackDirectory(folder, patterns)
		}
		return nil, packSourceErrorf("Neither %s nor a folder %s/ was found in %s.",
			src.tarballName(), filepath.Base(folder), s.dir)
	case urlsSource:
		folder := filepath.Join(s.dir, strings.TrimSuffix(src.Index, ".json"))
		if st, err := os.Stat(folder); err != nil || !st.IsDir() {
			return nil, packSourceErrorf("The folder %s/ was not found in %s.", filepath.Base(folder), s.dir)
		}
		found, err := readPackDirectory(folder, []string{"*.svg"})
		if err != nil {
			return nil, err
		}
		var missing []string
		expected := map[string]urlFile{}
		for _, file := range src.Files {
			expected[file.Name] = file
			if _, ok := found[file.Name]; !ok {
				missing = append(missing, file.Name)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, packSourceErrorf("%d of %d files are missing from %s/ (first: %s).",
				len(missing), len(expected), filepath.Base(folder), missing[0])
		}
		out := map[string][]byte{}
		for name, file := range expected {
			sum := sha256.Sum256(found[name])
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return nil, &packIntegrityError{packSourceError{message: fmt.Sprintf(
					"%s: the content does not match the pinned SHA-256", name)}}
			}
			out[name] = found[name]
		}
		return out, nil
	}
	return nil, packSourceErrorf("This source has no directory form.")
}

// httpPackSource is sources.HttpPackSource (npm tarballs and pinned files).
type httpPackSource struct{ client *http.Client }

func newHTTPPackSource() *httpPackSource {
	return &httpPackSource{client: &http.Client{Timeout: 60 * time.Second}}
}

func (s *httpPackSource) fetch(spec elementSource, patterns []string) (map[string][]byte, error) {
	switch src := spec.(type) {
	case npmSource:
		data, err := s.get(src.tarballURL(), packArchiveMaxBytes)
		if err != nil {
			var notFound *packNotFound
			if asPackNotFound(err, &notFound) {
				return nil, packSourceErrorf("%s was not found on the npm registry.", src.label())
			}
			return nil, err
		}
		if err := verifyPackIntegrity(data, src.Integrity, src.label()); err != nil {
			return nil, err
		}
		return readTarball(data, patterns)
	case urlsSource:
		out := map[string][]byte{}
		for _, file := range src.Files {
			data, err := s.get(file.URL, packURLFileMaxBytes)
			if err != nil {
				var notFound *packNotFound
				if asPackNotFound(err, &notFound) {
					return nil, packSourceErrorf("%s was not found at %s.", file.Name, file.URL)
				}
				return nil, err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return nil, &packIntegrityError{packSourceError{message: fmt.Sprintf(
					"%s: the content does not match the pinned SHA-256", file.Name)}}
			}
			out[file.Name] = data
		}
		return out, nil
	}
	return nil, packSourceErrorf("This source has no network form.")
}

// packNotFound marks a 404 so the caller can word the message.
type packNotFound struct{ url string }

func (e *packNotFound) Error() string { return "not found: " + e.url }

func asPackNotFound(err error, target **packNotFound) bool {
	n, ok := err.(*packNotFound)
	if ok {
		*target = n
	}
	return ok
}

// get is HttpPackSource._get: HTTPS + host allow-list, bounded retries on
// retryable statuses, hard size cap.
func (s *httpPackSource) get(rawURL string, limit int) ([]byte, error) {
	parsed, err := urlParse(rawURL)
	if err != nil || parsed.Scheme != "https" || !elementAllowedHosts[parsed.Hostname()] {
		return nil, packSourceErrorf("Refusing to download from %s.", parsed.Hostname())
	}
	var last error
	var hint time.Duration
	for attempt := 0; attempt < packAttempts; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<(attempt-1)) * time.Second
			if hint > wait {
				wait = hint
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			time.Sleep(wait)
			hint = 0
		}
		data, err := s.request(rawURL, limit)
		if err == nil {
			return data, nil
		}
		var notFound *packNotFound
		if asPackNotFound(err, &notFound) {
			return nil, err
		}
		var retry *packRetry
		if asPackRetry(err, &retry) {
			last, hint = err, retry.delay
			continue
		}
		last = err
	}
	if last != nil && isNetworkFailure(last) {
		return nil, &packSourceUnavailable{packSourceError{message: fmt.Sprintf(
			"Cannot reach %s: network error", parsed.Hostname())}}
	}
	if last != nil {
		return nil, packSourceErrorf("%v", last)
	}
	return nil, packSourceErrorf("Request to %s failed", rawURL)
}

type packRetry struct {
	message string
	delay   time.Duration
}

func (e *packRetry) Error() string { return e.message }

func asPackRetry(err error, target **packRetry) bool {
	r, ok := err.(*packRetry)
	if ok {
		*target = r
	}
	return ok
}

func isNetworkFailure(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "network")
}

// request is HttpPackSource._request.
func (s *httpPackSource) request(rawURL string, limit int) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, packSourceErrorf("%v", err)
	}
	request.Header.Set("User-Agent", packUserAgent)
	response, derr := s.client.Do(request)
	if derr != nil {
		return nil, &packSourceError{message: derr.Error()}
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, &packNotFound{url: rawURL}
	case response.StatusCode == 429 || response.StatusCode == 500 || response.StatusCode == 502 ||
		response.StatusCode == 503 || response.StatusCode == 504:
		delay := time.Duration(0)
		if seconds, perr := parseIntSafe(response.Header.Get("Retry-After")); perr == nil {
			delay = time.Duration(seconds) * time.Second
		}
		return nil, &packRetry{message: fmt.Sprintf("HTTP %d from %s", response.StatusCode, rawURL),
			delay: delay}
	case response.StatusCode != http.StatusOK:
		return nil, packSourceErrorf("HTTP %d from %s", response.StatusCode, rawURL)
	}
	data := make([]byte, 0, 4096)
	buf := make([]byte, 65536)
	for {
		n, rerr := response.Body.Read(buf)
		if n > 0 {
			if len(data)+n > limit {
				return nil, packSourceErrorf("The response from %s is larger than %d MB.",
					rawURL, limit/(1<<20))
			}
			data = append(data, buf[:n]...)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, &packSourceError{message: rerr.Error()}
		}
	}
	return data, nil
}

// generatedFiles is generated.generated_files: the pinned output of one
// generated set, `{folder/name.svg: bytes}`.
func generatedFiles(spec generatedSource) (map[string][]byte, error) {
	root := path.Join("data/elements_generated/graphics", spec.Set)
	entries, err := fs.ReadDir(generatedArtworkFS, root)
	if err != nil {
		return nil, packSourceErrorf("The generator has no set called %q.", spec.Set)
	}
	_ = entries
	out := map[string][]byte{}
	walkErr := fs.WalkDir(generatedArtworkFS, root, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, current)
		if rerr != nil {
			return nil
		}
		data, rerr := generatedArtworkFS.ReadFile(current)
		if rerr != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if walkErr != nil {
		return nil, packSourceErrorf("%v", walkErr)
	}
	if len(out) == 0 {
		return nil, packSourceErrorf("The generator has no set called %q.", spec.Set)
	}
	return out, nil
}

// generatedShapePackFiles is the seeded shapes: the pinned output of
// backend/data/generate_shapes.py, one folder per builtin pack.
func generatedShapePackFiles(slug string) (map[string][]byte, error) {
	root := path.Join("data/elements_generated", slug)
	out := map[string][]byte{}
	walkErr := fs.WalkDir(generatedArtworkFS, root, func(current string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, current)
		if rerr != nil {
			return nil
		}
		data, rerr := generatedArtworkFS.ReadFile(current)
		if rerr != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = data
		return nil
	})
	if walkErr != nil || len(out) == 0 {
		return nil, packSourceErrorf("The generated shapes for %q are missing.", slug)
	}
	return out, nil
}

// parseIntSafe is int() for an HTTP header.
func parseIntSafe(raw string) (int, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty")
	}
	var value int
	for _, r := range trimmed {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("not a number")
		}
		value = value*10 + int(r-'0')
	}
	return value, nil
}
