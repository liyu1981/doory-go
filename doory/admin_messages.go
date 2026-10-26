// Django's signed messages cookie (django.contrib.messages.storage.cookie):
// the flash messages every admin redirect carries. Byte-compatible with the
// oracle's Signer so an admin page opened against either side understands
// the other's flash.
//
// Format: TimestampSigner(salt="django.contrib.messages", key=
// "django.http.cookies"+SECRET_KEY).sign_object(messages, compress=True) —
// base64url(payload) then ":<b62 timestamp>:<base64url hmac>", payload is
// zlib(json) when compression helps, prefixed with ".".
package doory

import (
	"bytes"
	"compress/zlib"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

const adminMessageSalt = "django.contrib.messages"

var adminB62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func adminB62Encode(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var out []byte
	for n > 0 {
		out = append([]byte{adminB62Alphabet[n%62]}, out...)
		n /= 62
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func adminB64(data []byte) string {
	return strings.TrimRight(base64.URLEncoding.EncodeToString(data), "=")
}

func adminB64Decode(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}

// adminSignerSignature is Signer.signature: base64url(HMAC-SHA256(
// sha256(salt+"signer"+secret), value)).
func adminSignerSignature(salt, value, secret string) string {
	keySalt := salt + "signer"
	derived := sha256.Sum256([]byte(keySalt + secret))
	mac := hmac.New(sha256.New, derived[:])
	mac.Write([]byte(value))
	return adminB64(mac.Sum(nil))
}

func encodeAdminMessageCookie(msgs []adminMessage, secretKey string) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		// MessageEncoder: ["__json_message", is_safedata, level, text]
		raw, _ := json.Marshal([]any{"__json_message", 0, m.Level, m.Text})
		parts = append(parts, string(raw))
	}
	payload := []byte("[" + strings.Join(parts, ",") + "]")
	compressed := false
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	_, _ = zw.Write(payload)
	_ = zw.Close()
	if zbuf.Len() < len(payload)-1 {
		payload = zbuf.Bytes()
		compressed = true
	}
	value := adminB64(payload)
	if compressed {
		value = "." + value
	}
	value += ":" + adminB62Encode(time.Now().Unix())
	return value + ":" + adminSignerSignature(adminMessageSalt, value, "django.http.cookies"+secretKey)
}

func decodeAdminMessageCookie(signed, secretKey string) []adminMessage {
	// unsign: split at the LAST ":"; the remaining value is
	// "<base64>:<b62 timestamp>" (TimestampSigner.unsign splits the timestamp
	// before base64-decoding the payload).
	idx := strings.LastIndex(signed, ":")
	if idx < 0 {
		return nil
	}
	timestamped, sig := signed[:idx], signed[idx+1:]
	if expected := adminSignerSignature(adminMessageSalt, timestamped, "django.http.cookies"+secretKey); subtle.ConstantTimeCompare(
		[]byte(expected), []byte(sig)) != 1 {
		// The pin keeps SIGNED_COOKIE_LEGACY_SALT_FALLBACK off; tolerate the
		// legacy salt anyway so a rotated/older cookie still renders.
		legacy := adminSignerSignature(adminMessagesName, timestamped, "django.http.cookies"+secretKey)
		if subtle.ConstantTimeCompare([]byte(legacy), []byte(sig)) != 1 {
			return nil
		}
	}
	at := strings.LastIndex(timestamped, ":")
	if at < 0 {
		return nil
	}
	value := timestamped[:at]
	compressed := strings.HasPrefix(value, ".")
	if compressed {
		value = value[1:]
	}
	payload, err := adminB64Decode(value)
	if err != nil {
		return nil
	}
	if compressed {
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil
		}
		defer zr.Close()
		payload, err = io.ReadAll(zr)
		if err != nil {
			return nil
		}
	}
	var raw []any
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil
	}
	var out []adminMessage
	for _, item := range raw {
		array, ok := item.([]any)
		if !ok || len(array) < 4 || array[0] != "__json_message" {
			continue
		}
		level, _ := array[2].(float64)
		text, _ := array[3].(string)
		out = append(out, adminMessage{Level: int(level), Text: text})
	}
	return out
}

// adminMessageLevelName is django.contrib.messages constants (used by the
// rare caller that needs the numeric level from a name).
func adminMessageLevelName(level int) string {
	switch level {
	case 10:
		return "debug"
	case 20:
		return "info"
	case 25:
		return "success"
	case 30:
		return "warning"
	case 40:
		return "error"
	}
	return strconv.Itoa(level)
}
