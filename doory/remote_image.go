// Remote image fetching (apps/assets/remote.py): only public HTTPS hosts,
// every redirect hop checked, TLS pinned to the checked address, one deadline
// and a byte cap.
package doory

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	remoteImageUserAgent    = "Doory-StockProxy/1.0"
	remoteImageDeadline     = 25 * time.Second
	remoteImageConnect      = 3050 * time.Millisecond
	remoteImageRead         = 10 * time.Second
	remoteImageMaxURL       = 2048
	remoteImageMaxRedirects = 3
)

type remoteImageError struct{ message string }

func (e *remoteImageError) Error() string { return e.message }

type remoteImageTooLarge struct{ remoteImageError }

type remoteImageTarget struct {
	host      string
	addresses []netip.Addr
	path      string
}

// remoteLookup and remoteDo are replaceable in tests.
var remoteLookup = remoteResolve
var remoteDo = remoteDoReal
var fetchRemoteImage = remoteFetchImage

func remoteError(message string) error { return &remoteImageError{message: message} }

func remoteResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return nil, remoteError("Cannot find the host " + host + ".")
	}
	seen := map[netip.Addr]bool{}
	out := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if !seen[address] {
			seen[address] = true
			out = append(out, address)
		}
	}
	return out, nil
}

var remoteNAT64 = netip.MustParsePrefix("64:ff9b::/96")
var remoteTunnels = []netip.Prefix{
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func remoteIsPublic(address string) bool {
	addr, err := netip.ParseAddr(strings.Split(strings.Trim(address, "[]"), "%")[0])
	if err != nil {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	} else if addr.Is6() {
		if remoteNAT64.Contains(addr) {
			raw := addr.As16()
			addr = netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]})
		} else {
			for _, prefix := range remoteTunnels {
				if prefix.Contains(addr) {
					return false
				}
			}
		}
	}
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !addr.IsLoopback() &&
		!addr.IsLinkLocalUnicast() && !addr.IsLinkLocalMulticast() &&
		!addr.IsUnspecified() && !addr.IsMulticast()
}

func remoteCheckURL(ctx context.Context, rawURL string) (remoteImageTarget, error) {
	if len(rawURL) > remoteImageMaxURL {
		return remoteImageTarget{}, remoteError("The URL is too long or contains unsafe characters.")
	}
	for _, r := range rawURL {
		if r <= 0x20 || r >= 0x7f || r == '\\' {
			return remoteImageTarget{}, remoteError("The URL is too long or contains unsafe characters.")
		}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return remoteImageTarget{}, remoteError("That is not a valid URL.")
	}
	if strings.ToLower(parsed.Scheme) != "https" {
		return remoteImageTarget{}, remoteError("Only https URLs are allowed.")
	}
	if parsed.Hostname() == "" || parsed.User != nil {
		return remoteImageTarget{}, remoteError("The URL needs a host and no user name or password.")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return remoteImageTarget{}, remoteError("Only the standard https port is allowed.")
	}
	host := parsed.Hostname()
	var addresses []netip.Addr
	if addr, err := netip.ParseAddr(strings.Split(host, "%")[0]); err == nil {
		addresses = []netip.Addr{addr}
	} else {
		addresses, err = remoteLookup(ctx, host)
		if err != nil {
			return remoteImageTarget{}, err
		}
	}
	if len(addresses) == 0 {
		return remoteImageTarget{}, remoteError("That address is not on the public internet.")
	}
	for _, address := range addresses {
		if !remoteIsPublic(address.String()) {
			return remoteImageTarget{}, remoteError("That address is not on the public internet.")
		}
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return remoteImageTarget{host: host, addresses: addresses, path: path}, nil
}

type remoteHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func remoteDoReal(ctx context.Context, target remoteImageTarget) (remoteHTTPResponse, error) {
	dialer := &net.Dialer{Timeout: remoteImageConnect}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var last error
			for _, address := range target.addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), "443"))
				if err == nil {
					return conn, nil
				}
				last = err
			}
			if last == nil {
				last = fmt.Errorf("no address")
			}
			return nil, last
		},
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	client := &http.Client{Transport: transport, Timeout: remoteImageDeadline,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+target.host+target.path, nil)
	if err != nil {
		return remoteHTTPResponse{}, err
	}
	request.Header.Set("User-Agent", remoteImageUserAgent)
	request.Header.Set("Accept", "image/*")
	response, err := client.Do(request)
	if err != nil {
		return remoteHTTPResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUploadBytes+1))
	if err != nil {
		return remoteHTTPResponse{}, err
	}
	return remoteHTTPResponse{status: response.StatusCode, header: response.Header, body: body}, nil
}

func remoteFetchImage(rawURL string, maxBytes int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteImageDeadline)
	defer cancel()
	current := rawURL
	for hop := 0; hop <= remoteImageMaxRedirects; hop++ {
		if ctx.Err() != nil {
			return nil, remoteError("The download took too long.")
		}
		target, err := remoteCheckURL(ctx, current)
		if err != nil {
			return nil, err
		}
		response, err := remoteDo(ctx, target)
		if err != nil {
			if ctx.Err() != nil {
				return nil, remoteError("The download took too long.")
			}
			return nil, remoteError("The download failed.")
		}
		switch response.status {
		case 301, 302, 303, 307, 308:
			location := response.header.Get("Location")
			if location == "" {
				return nil, remoteError("The server redirected without a target.")
			}
			base, _ := url.Parse(current)
			next, err := url.Parse(location)
			if err != nil {
				return nil, remoteError("That is not a valid URL.")
			}
			current = base.ResolveReference(next).String()
			continue
		case 200:
			if !strings.HasPrefix(strings.ToLower(response.header.Get("Content-Type")), "image/") {
				return nil, remoteError("That URL is not an image.")
			}
			if declared := response.header.Get("Content-Length"); declared != "" {
				if n, err := strconv.Atoi(declared); err == nil && n > maxBytes {
					return nil, &remoteImageTooLarge{remoteImageError{"The image is too large."}}
				}
			}
			if len(response.body) > maxBytes {
				return nil, &remoteImageTooLarge{remoteImageError{"The image is too large."}}
			}
			return response.body, nil
		default:
			return nil, remoteError(fmt.Sprintf("The server answered %d.", response.status))
		}
	}
	return nil, remoteError("Too many redirects.")
}

func bareURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String()
}

// importRemoteImage ports the MCP upload_image_from_url asset path for URL
// values used by fill/bulk-fill.
func (a *App) importRemoteImage(owner *UserRow, rawURL, name string) (*assetRow, error) {
	if !a.Settings.McpExportURLUploads {
		return nil, fmt.Errorf("Uploading from a URL is switched off on this server.")
	}
	data, err := fetchRemoteImage(rawURL, maxUploadBytes)
	if err != nil {
		return nil, fmt.Errorf("%s Nothing was uploaded.", err)
	}
	if looksLikeAudio(data) {
		return nil, fmt.Errorf("That URL is not an image. Nothing was uploaded.")
	}
	prepared, perr := prepareRaster(data)
	if perr != nil {
		return nil, fmt.Errorf("%s Nothing was uploaded.", perr.Message)
	}
	if name == "" {
		parsed, _ := url.Parse(rawURL)
		name = filepath.Base(parsed.Path)
		if decoded, err := url.PathUnescape(name); err == nil {
			name = decoded
		}
		if name == "" || name == "." || name == "/" {
			name = "Image"
		}
	}
	assetName := cleanAssetName(name, "Image")
	asset := assetRow{
		ID: newUUID4Hex(), OwnerID: owner.ID, Kind: "image", Name: assetName,
		Width: int64(prepared.width), Height: int64(prepared.height),
		Size: int64(len(prepared.data)), Mime: prepared.mime,
		Tags: "[]", Meta: string(encodeCompactJSON(map[string]any{"source_url": bareURL(rawURL)})),
		Status: "ready",
	}
	asset.File = "assets/" + dashedUUID(owner.ID) + "/" + dashedUUID(asset.ID) + "." + prepared.ext
	asset.Thumbnail = "assets/" + dashedUUID(owner.ID) + "/thumbs/" + dashedUUID(asset.ID) + ".webp"
	used, _ := a.storageUsed(owner.ID)
	if used+asset.Size > a.Settings.UserStorageQuotaBytes {
		return nil, fmt.Errorf("Your storage quota is full. Delete some uploads to free up space.")
	}
	dir := filepath.Join(a.Settings.MediaRoot, filepath.Dir(asset.File))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(a.Settings.MediaRoot, asset.File), prepared.data, 0o644); err != nil {
		return nil, err
	}
	thumbPath := filepath.Join(a.Settings.MediaRoot, asset.Thumbnail)
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err == nil {
		_ = os.WriteFile(thumbPath, prepared.data, 0o644)
	}
	now := formatDjangoDateTime(time.Now())
	if _, err := a.DB.Exec(
		"INSERT INTO assets_asset ("+assetCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		asset.ID, asset.OwnerID, asset.Kind, asset.Name, asset.Width, asset.Height, asset.Size,
		asset.Mime, asset.Tags, asset.Meta, asset.File, asset.Thumbnail,
		asset.Poster, asset.Filmstrip, asset.Audio, asset.Status, asset.Error, now, now); err != nil {
		return nil, err
	}
	return a.getAssetByID(asset.ID)
}
