// Third-party audio packs (apps/assets/audiolib/packs.py): fetch a pinned zip
// archive, take only the members the manifest lists, verify each one's size,
// SHA-256 and container, and never extract by name (so a hostile archive
// cannot write outside the audio folder).

package doory

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	audioPackMaxUnpackedBytes = 512 * 1024 * 1024
	audioPackMaxRedirects     = 5
	audioPackUserAgent        = "Doory-audio-sync/1.0"
	audioPackAttempts         = 3
)

// audioPackSpec is audiolib.manifest.PackSpec (the fields the installer uses).
type audioPackSpec struct {
	ID            string
	Name          string
	Description   string
	Homepage      string
	License       audioLicense
	Attribution   string
	ArchiveURL    string
	ArchiveBytes  int64
	ArchiveSHA256 string
	ArchiveName   string
	Tracks        []audioTrack
}

// audioPackError / audioPackUnavailable mirror packs.py's exceptions.
type audioPackError struct{ message string }

func (e *audioPackError) Error() string { return e.message }

type audioPackUnavailable struct{ audioPackError }

func audioPackErrorf(format string, args ...any) error {
	return &audioPackError{message: fmt.Sprintf(format, args...)}
}

func asAudioPackUnavailable(err error, target **audioPackUnavailable) bool {
	u, ok := err.(*audioPackUnavailable)
	if ok {
		*target = u
	}
	return ok
}

// audioPackSource is packs.PackSource: write the pack's archive to destination.
type audioPackSource interface {
	fetch(pack audioPackSpec, destination string) error
}

// audioDirectoryPackSource is packs.DirectoryPackSource (--from-dir).
type audioDirectoryPackSource struct{ dir string }

func (s audioDirectoryPackSource) fetch(pack audioPackSpec, destination string) error {
	for _, name := range []string{pack.ArchiveName, pack.ID + ".zip"} {
		candidate := path.Join(s.dir, name)
		data, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		return os.WriteFile(destination, data, 0o644)
	}
	return audioPackErrorf("%s is not in %s", pack.ArchiveName, s.dir)
}

// audioHTTPPackSource is packs.HttpPackSource.
type audioHTTPPackSource struct{ client *http.Client }

func newAudioHTTPPackSource() *audioHTTPPackSource {
	return &audioHTTPPackSource{client: &http.Client{Timeout: 60 * time.Second}}
}

func (s *audioHTTPPackSource) fetch(pack audioPackSpec, destination string) error {
	var last error
	hint := time.Duration(0)
	for attempt := 0; attempt < audioPackAttempts; attempt++ {
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
		err := s.download(pack, destination)
		if err == nil {
			return nil
		}
		var retry *audioPackRetry
		if asAudioPackRetry(err, &retry) {
			last, hint = err, retry.delay
			continue
		}
		last = err
	}
	host := hostOf(pack.ArchiveURL)
	if last != nil && isNetworkFailure(last) {
		return &audioPackUnavailable{audioPackError{message: fmt.Sprintf("Cannot reach %s: network error", host)}}
	}
	if last != nil {
		return audioPackErrorf("%s: %v", host, last)
	}
	return audioPackErrorf("Download from %s failed", host)
}

type audioPackRetry struct {
	message string
	delay   time.Duration
}

func (e *audioPackRetry) Error() string { return e.message }

func asAudioPackRetry(err error, target **audioPackRetry) bool {
	r, ok := err.(*audioPackRetry)
	if ok {
		*target = r
	}
	return ok
}

// download follows redirects by hand (allowed hosts only) with a hard size cap.
func (s *audioHTTPPackSource) download(pack audioPackSpec, destination string) error {
	current := pack.ArchiveURL
	for hop := 0; hop <= audioPackMaxRedirects; hop++ {
		parsed, err := urlParse(current)
		if err != nil || parsed.Scheme != "https" || !audioHostAllowed(parsed.Hostname()) {
			return audioPackErrorf("Refusing to download from %s.", parsed.Hostname())
		}
		request, rerr := http.NewRequest(http.MethodGet, current, nil)
		if rerr != nil {
			return audioPackErrorf("%v", rerr)
		}
		request.Header.Set("User-Agent", audioPackUserAgent)
		response, derr := s.client.Do(request)
		if derr != nil {
			return &audioPackUnavailable{audioPackError{message: fmt.Sprintf("Cannot reach %s: network error",
				parsed.Hostname())}}
		}
		switch response.StatusCode {
		case 301, 302, 303, 307, 308:
			location := response.Header.Get("Location")
			response.Body.Close()
			if location == "" {
				return audioPackErrorf("Redirect without a target from %s.", parsed.Hostname())
			}
			next, perr := response.Request.URL.Parse(location)
			if perr != nil {
				return audioPackErrorf("%v", perr)
			}
			current = next.String()
			continue
		case 429, 500, 502, 503, 504:
			delay := time.Duration(0)
			if seconds, perr := parseIntSafe(response.Header.Get("Retry-After")); perr == nil {
				delay = time.Duration(seconds) * time.Second
			}
			response.Body.Close()
			return &audioPackRetry{message: fmt.Sprintf("HTTP %d", response.StatusCode), delay: delay}
		case 200:
			file, ferr := os.Create(destination)
			if ferr != nil {
				response.Body.Close()
				return audioPackErrorf("%v", ferr)
			}
			written := int64(0)
			buf := make([]byte, 256*1024)
			for {
				n, rerr := response.Body.Read(buf)
				if n > 0 {
					written += int64(n)
					if written > pack.ArchiveBytes {
						file.Close()
						response.Body.Close()
						return audioPackErrorf("The download is larger than the manifest says.")
					}
					if _, werr := file.Write(buf[:n]); werr != nil {
						file.Close()
						response.Body.Close()
						return audioPackErrorf("%v", werr)
					}
				}
				if rerr == io.EOF {
					break
				}
				if rerr != nil {
					file.Close()
					response.Body.Close()
					return audioPackErrorf("%v", rerr)
				}
			}
			file.Close()
			response.Body.Close()
			return nil
		default:
			response.Body.Close()
			return audioPackErrorf("%s: HTTP %d", parsed.Hostname(), response.StatusCode)
		}
	}
	return audioPackErrorf("Too many redirects.")
}

// audioHostAllowed is manifest.host_allowed for the audio manifest's hosts.
var audioAllowedHosts = []string{"github.com", "zenodo.org", "archive.org", "opengameart.org"}
var audioAllowedHostSuffixes = []string{".githubusercontent.com", ".archive.org", ".zenodo.org", ".opengameart.org"}

func audioHostAllowed(host string) bool {
	for _, allowed := range audioAllowedHosts {
		if host == allowed {
			return true
		}
	}
	for _, suffix := range audioAllowedHostSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// verifyAudioArchive is packs.verify_archive.
func verifyAudioArchive(pack audioPackSpec, archivePath string) error {
	st, err := os.Stat(archivePath)
	if err != nil {
		return audioPackErrorf("%s: %v", pack.ArchiveName, err)
	}
	if st.Size() != pack.ArchiveBytes {
		return audioPackErrorf("%s: %d bytes, the manifest pins %d", pack.ArchiveName, st.Size(),
			pack.ArchiveBytes)
	}
	sum, herr := sha256Of(archivePath)
	if herr != nil {
		return audioPackErrorf("%s: %v", pack.ArchiveName, herr)
	}
	if sum != pack.ArchiveSHA256 {
		return audioPackErrorf("%s: the content does not match the pinned SHA-256", pack.ArchiveName)
	}
	return nil
}

// readAudioMembers is packs.read_members: the wanted tracks' bytes, verified.
func readAudioMembers(archivePath string, pack audioPackSpec, wanted []audioTrack) (map[string][]byte, error) {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return nil, audioPackErrorf("%s is not a valid zip file", pack.ArchiveName)
	}
	defer reader.Close()
	infos := map[string]*zip.File{}
	total := uint64(0)
	for _, file := range reader.File {
		infos[file.Name] = file
		total += file.UncompressedSize64
	}
	if total > audioPackMaxUnpackedBytes {
		return nil, audioPackErrorf("%s unpacks to more than %d MB", pack.ArchiveName,
			audioPackMaxUnpackedBytes/(1<<20))
	}
	out := map[string][]byte{}
	for _, track := range wanted {
		info, ok := infos[track.File]
		if !ok || strings.HasSuffix(info.Name, "/") {
			return nil, audioPackErrorf("%s is not in %s", track.File, pack.ArchiveName)
		}
		// (external_attr >> 16) & 0o170000 == 0o120000 is a symbolic link.
		if (info.ExternalAttrs>>16)&0o170000 == 0o120000 {
			return nil, audioPackErrorf("%s is a link, not a file", track.File)
		}
		if int64(info.UncompressedSize64) != track.Bytes || int64(info.UncompressedSize64) > audioMaxFileBytes {
			return nil, audioPackErrorf("%s: %d bytes, the manifest pins %d", track.File,
				info.UncompressedSize64, track.Bytes)
		}
		handle, herr := info.Open()
		if herr != nil {
			return nil, audioPackErrorf("%v", herr)
		}
		data, rerr := io.ReadAll(io.LimitReader(handle, track.Bytes+1))
		handle.Close()
		if rerr != nil {
			return nil, audioPackErrorf("%v", rerr)
		}
		sum := sha256.Sum256(data)
		if int64(len(data)) != track.Bytes || hex.EncodeToString(sum[:]) != track.SHA256 {
			return nil, audioPackErrorf("%s: the content does not match the pinned SHA-256", track.File)
		}
		if problem := validateAudioContainer(data, track.ext()); problem != "" {
			return nil, audioPackErrorf("%s: not a valid audio file (%s)", track.File, problem)
		}
		out[track.ID] = data
	}
	return out, nil
}

// installAudioPack is install._pack_tracks: download/verify the archive, read
// the listed members and install them; returns `{track id: relative path}` of
// the tracks in place afterwards.
func (a *App) installAudioPack(pack audioPack, source audioPackSource, root string, options audioOptions,
	report *audioReport, log func(string),
) (map[string]string, error) {
	spec := pack.spec()
	relative := map[string]string{}
	for _, track := range pack.Tracks {
		relative[track.ID] = "packs/" + pack.ID + "/" + track.storedName()
	}
	var missing []audioTrack
	for _, track := range pack.Tracks {
		if options.Force || !audioIntactIn(root, track, relative[track.ID]) {
			missing = append(missing, track)
		}
	}
	if len(missing) == 0 {
		for range pack.Tracks {
			report.UpToDate++
		}
		return relative, nil
	}
	scratch, err := os.MkdirTemp("", "doory-pack-")
	if err != nil {
		return nil, audioPackErrorf("%v", err)
	}
	defer os.RemoveAll(scratch)
	archivePath := filepath.Join(scratch, spec.ArchiveName)
	if err := source.fetch(spec, archivePath); err != nil {
		return nil, err
	}
	if err := verifyAudioArchive(spec, archivePath); err != nil {
		return nil, err
	}
	members, err := readAudioMembers(archivePath, spec, missing)
	if err != nil {
		return nil, err
	}
	for _, track := range pack.Tracks {
		data, ok := members[track.ID]
		if !ok {
			report.UpToDate++
			continue
		}
		if options.DryRun {
			report.WouldInstall++
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(relative[track.ID]))
		if err := writeFileAtomic(target, data); err != nil {
			return nil, audioPackErrorf("%v", err)
		}
		report.Installed++
	}
	return relative, nil
}

// audioIntactIn is install._intact for a pack track at a relative path.
func audioIntactIn(root string, track audioTrack, relative string) bool {
	target := filepath.Join(root, filepath.FromSlash(relative))
	st, err := os.Stat(target)
	if err != nil || !st.Mode().IsRegular() || st.Size() != track.Bytes {
		return false
	}
	sum, herr := sha256Of(target)
	return herr == nil && sum == track.SHA256
}

// audioPackCatalogueEntry is the `packs` row of index.json.
func audioPackCatalogueEntry(pack audioPack) map[string]any {
	return map[string]any{
		"id": pack.ID, "name": pack.Name, "homepage": pack.Homepage,
		"license": audioLicenseJSON(pack.License), "attribution": pack.Attribution,
	}
}

// audioLicenseJSON is LicenseSpec.as_json (note omitted when empty).
func audioLicenseJSON(license audioLicense) map[string]any {
	out := map[string]any{"spdx": license.SPDX, "url": license.URL, "holder": license.Holder}
	if license.Note != "" {
		out["note"] = license.Note
	}
	return out
}
