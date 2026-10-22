// Package aikeys ports apps/ai/secrets.py: Fernet encryption of provider API keys
// at rest, with the key derived from Django's SECRET_KEY via HKDF when no explicit
// AI_SECRETS_KEY is configured.
//
// Fernet internals recovered from cryptography (Fernet.__init__): the b64url key
// decodes to 32 bytes; signing = key[:16], encryption = key[16:] (AES-128-CBC +
// HMAC-SHA256, token = 0x80 | timestamp | IV | ciphertext | HMAC). Proof: two
// fixtures encrypted by the oracle's own cryptography library under the parity
// SECRET_KEY decrypt here to their plaintexts, and a token encrypted here
// decrypts under cryptography (cross-impl, checked at M10).
package aikeys

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

var (
	// SaltSalt and info match secrets.py.
	hkdfSalt = []byte("doory.ai.secrets")
	hkdfInfo = []byte("provider secrets v1")

	// ErrUnreadable: no configured key can decrypt the token (secrets.SecretUnreadable).
	ErrUnreadable = errors.New("aikeys: token cannot be read with any configured key")
)

// DeriveKey ports secrets.derive_key: HKDF-SHA256(32) over the secret key string,
// returned as urlsafe base64 (the Fernet key encoding).
func DeriveKey(secretKey string) []byte {
	// HKDF-Extract(salt) then Expand(info) — RFC 5869.
	prk := hmacSHA256(hkdfSalt, []byte(secretKey))
	okm := hkdfExpand(prk, hkdfInfo, 32)
	out := make([]byte, base64.URLEncoding.EncodedLen(len(okm)))
	base64.URLEncoding.Encode(out, okm)
	return out
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

func hkdfExpand(prk, info []byte, length int) []byte {
	var out, t []byte
	for i := byte(1); len(out) < length; i++ {
		m := hmac.New(sha256.New, prk)
		m.Write(t)
		m.Write(info)
		m.Write([]byte{i})
		t = m.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// KeyMaterial decodes a Fernet key string to (signing, encryption) 16-byte halves.
// An invalid key string raises the equivalent of cryptography's ValueError.
func KeyMaterial(key string) (sign, enc []byte, err error) { return keyMaterial(key) }

func keyMaterial(key string) (sign, enc []byte, err error) {
	raw, err := base64.URLEncoding.DecodeString(key)
	if err != nil {
		// cryptography accepts keys without padding? Fernet keys are 43-char unpadded;
		// use RawURLEncoding fallback.
		raw, err = base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			return nil, nil, fmt.Errorf("aikeys: Fernet key must be 32 url-safe base64-encoded bytes")
		}
	}
	if len(raw) != 32 {
		return nil, nil, fmt.Errorf("aikeys: Fernet key must be 32 url-safe base64-encoded bytes")
	}
	return raw[:16], raw[16:], nil
}

// Decrypt ports MultiFernet.decrypt over the given keys (primary first): verifies
// the HMAC, then AES-128-CBC decrypts and strips PKCS7 padding. ttl <= 0: no
// expiry check (Fernet verify_ttl defaults on in MultiFernet — we accept tokens
// regardless of age here, matching decrypt(token) usage in secrets.py where no
// ttl is passed... see secrets.decrypt: Fernet.decrypt(token) with default ttl
// None → no expiry).
func Decrypt(token []byte, keys []string) ([]byte, error) {
	raw, err := base64.URLEncoding.DecodeString(string(token))
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(string(token))
		if err != nil {
			return nil, ErrUnreadable
		}
	}
	if len(raw) < 1+8+16+32 || raw[0] != 0x80 {
		return nil, ErrUnreadable
	}
	body, mac := raw[:len(raw)-32], raw[len(raw)-32:]
	var lastErr error = ErrUnreadable
	for _, key := range keys {
		sign, encKey, err := keyMaterial(key)
		if err != nil {
			lastErr = err
			continue
		}
		want := hmacSHA256(sign, body)
		if subtle.ConstantTimeCompare(want, mac) != 1 {
			lastErr = ErrUnreadable
			continue
		}
		iv, ct := body[9:25], body[25:]
		if len(ct)%aes.BlockSize != 0 {
			lastErr = ErrUnreadable
			continue
		}
		block, err := aes.NewCipher(encKey)
		if err != nil {
			lastErr = err
			continue
		}
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
		pad := int(pt[len(pt)-1])
		if pad == 0 || pad > aes.BlockSize || pad > len(pt) {
			lastErr = ErrUnreadable
			continue
		}
		if subtle.ConstantTimeCompare(pt[len(pt)-pad:], bytesOf(pad)) != 1 {
			lastErr = ErrUnreadable
			continue
		}
		return pt[:len(pt)-pad], nil
	}
	return nil, lastErr
}

func bytesOf(pad int) []byte {
	b := make([]byte, pad)
	for i := range b {
		b[i] = byte(pad)
	}
	return b
}

// Encrypt ports MultiFernet.encrypt: 0x80 | timestamp | random IV | AES-128-CBC |
// HMAC over the head, base64url unpadded.
func Encrypt(plaintext, key string) (string, error) {
	sign, encKey, err := keyMaterial(key)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 16)
	if _, err := crand.Read(iv); err != nil {
		return "", err
	}
	// PKCS7 pad (16).
	pt := []byte(plaintext)
	pad := aes.BlockSize - len(pt)%aes.BlockSize
	for i := 0; i < pad; i++ {
		pt = append(pt, byte(pad))
	}
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)

	head := make([]byte, 1+8+16)
	head[0] = 0x80
	binary.BigEndian.PutUint64(head[1:9], uint64(time.Now().Unix()))
	copy(head[9:], iv)
	body := append(head, ct...)
	mac := hmacSHA256(sign, body)
	// Fernet tokens are padded urlsafe base64 (cryptography's b64encode).
	return base64.URLEncoding.EncodeToString(append(body, mac...)), nil
}
