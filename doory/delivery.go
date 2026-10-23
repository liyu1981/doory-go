// Signed, expiring delivery links (apps/exports/delivery.py) — the
// Django-signing format ported byte-compatibly (HMAC-SHA256 with the
// salted-key derivation, base62 timestamps, optional zlib compression
// flagged by a leading "."), so link tokens interoperate across stacks
// (D5 groundwork — verified by unit evidence against Django-made tokens).
//
// Ports backend/apps/exports/delivery.py (store/open semantics)
// Ports django/core/signing.py (Signer/TimestampSigner format)
package doory

import (
	"bytes"
	"compress/zlib"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const deliverySalt = "doory.exports.image-link"

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var deliveryNameRe = regexp.MustCompile(`^(\d{10})-[0-9a-f]{32}\.(png|jpg|webp)$`)

var deliveryMIMEs = map[string]string{"png": "image/png", "jpg": "image/jpeg", "webp": "image/webp"}

func b62Encode(v int64) string {
	if v == 0 {
		return "0"
	}
	var out []byte
	for v > 0 {
		out = append([]byte{base62Alphabet[v%62]}, out...)
		v /= 62
	}
	return string(out)
}

func b62Decode(s string) (int64, bool) {
	if s == "0" {
		return 0, true
	}
	var v int64
	for _, c := range s {
		idx := bytes.IndexRune([]byte(base62Alphabet), c)
		if idx < 0 {
			return 0, false
		}
		v = v*62 + int64(idx)
	}
	return v, true
}

// signValue ports Signer.signature: base64url(hmac-sha256(key, value)) with
// key = sha256((salt + "signer") + SECRET_KEY) — salted_hmac's derivation.
func signValue(saltSuffix, value, secret string) string {
	keySalt := deliverySalt + saltSuffix
	derived := sha256.Sum256([]byte(keySalt + secret))
	mac := hmac.New(sha256.New, derived[:])
	mac.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signToken ports TimestampSigner.sign over sign_object(compress=True):
// "<payload>.<ts>:<sig>".
func signToken(payload []byte, secret string, now time.Time) string {
	compressed := zlibCompress(payload)
	data := base64.RawURLEncoding.EncodeToString(payload)
	isCompressed := len(compressed) < len(payload)-1
	if isCompressed {
		data = "." + base64.RawURLEncoding.EncodeToString(compressed)
	}
	value := fmt.Sprintf("%s:%s", data, b62Encode(now.Unix()))
	return value + ":" + signValue("signer", value, secret)
}

// verifyToken ports TimestampSigner.unsign_object with max_age: the payload
// bytes or nil (forged/expired/gone).
func verifyToken(token string, secret string, maxAgeSeconds int, now time.Time) []byte {
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 {
		return nil
	}
	value := parts[0] + ":" + parts[1]
	if signValue("signer", value, secret) != parts[2] {
		return nil
	}
	ts, ok := b62Decode(parts[1])
	if !ok || now.Unix()-int64(ts) > int64(maxAgeSeconds) {
		return nil // expired
	}
	data := parts[0]
	compressed := false
	if len(data) > 0 && data[0] == '.' {
		compressed = true
		data = data[1:]
	}
	raw, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	if compressed {
		raw, err = zlibDecompress(raw)
		if err != nil {
			return nil
		}
	}
	return raw
}

func zlibCompress(data []byte) []byte {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write(data)
	_ = zw.Close()
	return buf.Bytes()
}

func zlibDecompress(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// exportStorageRoot resolves EXPORT_STORAGE_ROOT (private, never under
// /media/).
func (a *App) exportStorageRoot() string {
	return a.Settings.ExportStorageRoot
}

// storeImage ports delivery.store_image: purge expired, store under the
// timestamped name, return the signed SITE_URL link.
func (a *App) storeImage(data []byte, mime string, now time.Time) (string, error) {
	a.purgeExpiredExports(now)
	ext := ""
	for e, m := range deliveryMIMEs {
		if m == mime {
			ext = e
		}
	}
	if ext == "" {
		return "", fmt.Errorf("unsupported mime %q", mime)
	}
	name := fmt.Sprintf("%010d-%s.%s", now.Unix(), newUUID4Hex(), ext)
	root := a.exportStorageRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]string{"name": name})
	token := signToken(payload, a.Settings.SecretKey, now)
	return strings.TrimRight(a.Settings.SiteURL, "/") + "/api/v1/exports/image/" + token + "/", nil
}

// openLink ports delivery.open_link: forged, expired and gone links all
// return false (404s never say why).
func (a *App) openLink(token string) (string, string, bool) {
	payload := verifyToken(token, a.Settings.SecretKey, a.Settings.McpExportURLTTLSeconds, time.Now())
	if payload == nil {
		return "", "", false
	}
	var body struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(payload, &body) != nil {
		return "", "", false
	}
	match := deliveryNameRe.FindStringSubmatch(body.Name)
	if match == nil {
		return "", "", false
	}
	if _, err := os.Stat(filepath.Join(a.exportStorageRoot(), body.Name)); err != nil {
		return "", "", false
	}
	return body.Name, deliveryMIMEs[match[2]], true
}

// purgeExpiredExports ports delivery.purge_expired: name-encoded timestamps
// decide age; files older than the TTL go.
func (a *App) purgeExpiredExports(now time.Time) int {
	cutoff := now.Unix() - int64(a.Settings.McpExportURLTTLSeconds)
	root := a.exportStorageRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	deleted := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := deliveryNameRe.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		var stamp int64
		for _, c := range match[1] {
			stamp = stamp*10 + int64(c-'0')
		}
		if stamp < cutoff {
			if err := os.Remove(filepath.Join(root, entry.Name())); err == nil {
				deleted++
			}
		}
	}
	return deleted
}
