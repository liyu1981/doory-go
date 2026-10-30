package doory

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/netip"
	"strings"
	"testing"
)

func imageBytesForRemote(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 20), uint8(y * 20), 90, 255})
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRemoteCheckURLRefusesPrivateAndOddAddresses(t *testing.T) {
	oldLookup := remoteLookup
	t.Cleanup(func() { remoteLookup = oldLookup })
	remoteLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "public.example":
			return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
		case "mixed.example":
			return []netip.Addr{netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("10.0.0.1")}, nil
		case "private.example":
			return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
		}
		return nil, remoteError("Cannot find the host " + host + ".")
	}
	for _, bad := range []string{
		"http://public.example/a.png",
		"https://public.example:8443/a.png",
		"https://user:pw@public.example/a.png",
		"https://127.0.0.1/a.png",
		"https://169.254.169.254/latest/meta-data",
		"https://[::1]/a.png",
		"https://[::ffff:10.0.0.1]/a.png",
		"https://[64:ff9b::a9fe:a9fe]/a.png",
		"https://private.example/a.png",
		"https://mixed.example/a.png",
		"file:///etc/passwd",
	} {
		if _, err := remoteCheckURL(context.Background(), bad); err == nil {
			t.Errorf("remoteCheckURL(%q) accepted", bad)
		}
	}
	for _, good := range []string{
		"https://public.example/a.png",
		"https://public.example/a.png?x=1",
		"https://[64:ff9b::5db8:d70e]/a.png",
	} {
		if _, err := remoteCheckURL(context.Background(), good); err != nil {
			t.Errorf("remoteCheckURL(%q): %v", good, err)
		}
	}
	if !remoteIsPublic("64:ff9b::5db8:d70e") || remoteIsPublic("64:ff9b::a00:1") {
		t.Fatal("NAT64 public/private classification is wrong")
	}
}

func TestRemoteFetchRedirectsAndValidates(t *testing.T) {
	oldLookup, oldDo := remoteLookup, remoteDo
	t.Cleanup(func() { remoteLookup, remoteDo = oldLookup, oldDo })
	remoteLookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		if host == "evil.example" {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("93.184.215.14")}, nil
	}
	pngBytes := imageBytesForRemote(t)
	var calls []string
	remoteDo = func(ctx context.Context, target remoteImageTarget) (remoteHTTPResponse, error) {
		calls = append(calls, target.host)
		switch target.host {
		case "img.example":
			return remoteHTTPResponse{status: 302, header: http.Header{"Location": []string{"https://cdn.example/b.png"}}}, nil
		case "cdn.example":
			return remoteHTTPResponse{status: 200, header: http.Header{"Content-Type": []string{"image/png"}}, body: pngBytes}, nil
		case "large.example":
			return remoteHTTPResponse{status: 200, header: http.Header{"Content-Type": []string{"image/png"}, "Content-Length": []string{"9999"}}, body: pngBytes}, nil
		case "html.example":
			return remoteHTTPResponse{status: 200, header: http.Header{"Content-Type": []string{"text/html"}}}, nil
		case "loop.example":
			return remoteHTTPResponse{status: 302, header: http.Header{"Location": []string{"/again"}}}, nil
		}
		return remoteHTTPResponse{status: 404}, nil
	}
	data, err := remoteFetchImage("https://img.example/a.png", 1024*1024)
	if err != nil || !bytes.Equal(data, pngBytes) || strings.Join(calls, ",") != "img.example,cdn.example" {
		t.Fatalf("redirect fetch: data=%d err=%v calls=%v", len(data), err, calls)
	}
	if _, err := remoteFetchImage("https://img.example/a.png", 1); err == nil {
		t.Fatal("body over the cap was accepted")
	}
	if _, err := remoteFetchImage("https://large.example/a.png", 100); err == nil {
		t.Fatal("declared content-length over the cap was accepted")
	}
	if _, err := remoteFetchImage("https://html.example/a.png", 1024); err == nil {
		t.Fatal("non-image content type was accepted")
	}
	if _, err := remoteFetchImage("https://loop.example/a.png", 1024); err == nil ||
		!strings.Contains(err.Error(), "Too many redirects") {
		t.Fatalf("redirect loop error = %v", err)
	}
	if _, err := remoteFetchImage("https://evil.example/a.png", 1024); err == nil {
		t.Fatal("private redirect target was accepted")
	}
}

func TestImportRemoteImageStoresAnAsset(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("url@example.com", false, false)
	oldFetch := fetchRemoteImage
	fetchRemoteImage = func(rawURL string, maxBytes int) ([]byte, error) {
		return imageBytesForRemote(t), nil
	}
	t.Cleanup(func() { fetchRemoteImage = oldFetch })
	asset, err := env.app.importRemoteImage(user, "https://img.example/a.png?token=secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if asset == nil || asset.Kind != "image" || asset.Name != "a.png" {
		t.Fatalf("asset = %+v", asset)
	}
	if !strings.Contains(asset.Meta, `"source_url":"https://img.example/a.png"`) ||
		strings.Contains(asset.Meta, "secret") {
		t.Fatalf("meta = %s", asset.Meta)
	}
}
