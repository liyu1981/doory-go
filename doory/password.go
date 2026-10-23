// Password hashing and validation.
//
// Hashing ports django.contrib.auth.hashers.PBKDF2PasswordHasher:
// pbkdf2_sha256$<iterations>$<12-char salt>$<base64 dk32> with the pinned
// iteration count 1500000 (probed from oracle rows, probes/0002-accounts/
// password-hashes.txt). Verification accepts the same encoding, so D5
// (Django session/password interop) stays open with a working verifier.
//
// Validation ports django.contrib.auth.password_validation.validate_password
// in settings order (base.py AUTH_PASSWORD_VALIDATORS): similarity,
// minimum-length(8), common, numeric — messages and codes byte-proven by
// probes/0002-accounts (password_too_short, password_too_common +
// password_entirely_numeric order; password_too_similar per validator
// source at the pin).
//
// Ports backend/apps/accounts/serializers.py (check_new_password) as the
// caller; the validator semantics themselves are Django framework behavior
// pinned by the oracle probe.
package doory

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"sync"
	"unicode"
)

const (
	pbkdf2Algorithm   = "pbkdf2_sha256"
	pbkdf2Iterations  = 1500000
	pbkdf2SaltLen     = 12 // Django get_random_string(12): raw ascii in the encoded form
	pbkdf2KeyLen      = 32
	passwordMaxLength = 128 // PASSWORD_MAX_LENGTH (serializers.py): bounds hashing cost
	minPasswordLength = 8   // base.py AUTH_PASSWORD_VALIDATORS MinimumLength min_length
)

//go:embed data/common-passwords.txt
var commonPasswordsData []byte

// hashPassword encodes like Django's PBKDF2PasswordHasher.encode: the salt
// is 12 chars drawn from ASCII alphanumerics and sits RAW in the encoded
// form (probed: pbkdf2_sha256$1500000$GKwsGdcksunGaaJ8Kt9v3c$...).
func hashPassword(raw string) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	rnd := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(rnd); err != nil {
		panic(err)
	}
	salt := make([]byte, pbkdf2SaltLen)
	for i, b := range rnd {
		salt[i] = alphabet[int(b)%len(alphabet)]
	}
	dk := pbkdf2SHA256([]byte(raw), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return pbkdf2Algorithm + "$" + itoa(pbkdf2Iterations) + "$" + string(salt) + "$" +
		base64.StdEncoding.EncodeToString(dk)
}

// verifyPassword checks raw against a Django pbkdf2_sha256 encoding
// (constant-time). Other/unknown algorithms return false.
func verifyPassword(raw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != pbkdf2Algorithm {
		return false
	}
	iterations := atoi(parts[1])
	if iterations <= 0 {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(raw), []byte(parts[2]), iterations, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// hasUsablePassword ports User.has_usable_password(): a set (hashable)
// password exists — Google-only accounts carry Django's unusable marker.
func hasUsablePassword(encoded string) bool {
	return encoded != "" && !strings.HasPrefix(encoded, "!")
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) with HMAC-SHA256.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := prf.Size()
	numBlocks := (keyLen + hashLen - 1) / hashLen
	dk := make([]byte, 0, numBlocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 2; n <= iter; n++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for x := range t {
				t[x] ^= u[x]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

// PasswordError is one validator failure (message + Django code).
type PasswordError struct {
	Message string
	Code    string
}

// validateNewPassword runs Django's validators in base.py settings order
// against a candidate with the given email (similarity reads the email
// attribute; our User has no first/last name fields). Returns ALL failures
// (Django collects, never short-circuits).
func validateNewPassword(password, email string) []PasswordError {
	var errs []PasswordError
	lower := strings.ToLower(password)

	// 1. UserAttributeSimilarityValidator (max_similarity 0.7) over the
	// email attribute and its \W+ parts — SequenceMatcher.quick_ratio as a
	// character-multiset ratio.
	if email != "" {
		value := strings.ToLower(email)
		parts := append(splitNonWord(value), value)
		for _, part := range parts {
			if part == "" {
				continue
			}
			if exceedsMaximumLengthRatio(password, 0.7, part) {
				continue
			}
			if quickRatio(lower, part) >= 0.7 {
				errs = append(errs, PasswordError{
					Message: "The password is too similar to the email.",
					Code:    "password_too_similar",
				})
				break
			}
		}
	}

	// 2. MinimumLengthValidator (min_length 8, base.py).
	if len([]rune(password)) < minPasswordLength {
		errs = append(errs, PasswordError{
			Message: "This password is too short. It must contain at least 8 characters.",
			Code:    "password_too_short",
		})
	}

	// 3. CommonPasswordValidator (Django's shipped list, embedded as
	// data/common-passwords.txt; comparison lowercases both sides).
	if commonPasswords()[lower] {
		errs = append(errs, PasswordError{
			Message: "This password is too common.",
			Code:    "password_too_common",
		})
	}

	// 4. NumericPasswordValidator (Python str.isdigit semantics).
	if isEntirelyNumeric(password) {
		errs = append(errs, PasswordError{
			Message: "This password is entirely numeric.",
			Code:    "password_entirely_numeric",
		})
	}
	return errs
}

// exceedsMaximumLengthRatio ports password_validation.exceeds_maximum_length_ratio.
func exceedsMaximumLengthRatio(password string, maxSimilarity float64, value string) bool {
	pwdLen := float64(len([]rune(password)))
	valueLen := float64(len([]rune(value)))
	return pwdLen >= 10*valueLen && valueLen < maxSimilarity/2*pwdLen
}

// quickRatio ports SequenceMatcher.quick_ratio:2 * multiset-intersection /
// total length (the maximum achievable ratio, per cheap character counts).
func quickRatio(a, b string) float64 {
	ca := map[rune]int32{}
	for _, r := range a {
		ca[r]++
	}
	cb := map[rune]int32{}
	for _, r := range b {
		cb[r]++
	}
	var inter int32
	for r, n := range ca {
		if m := cb[r]; m < n {
			inter += m
		} else {
			inter += n
		}
	}
	la := len([]rune(a))
	lb := len([]rune(b))
	if la+lb == 0 {
		return 1
	}
	return 2 * float64(inter) / float64(la+lb)
}

// splitNonWord splits like Python re.split(r"\W+", s): word chars are
// unicode letters, digits and underscore (Python 3 default \w semantics).
func splitNonWord(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// isEntirelyNumeric mirrors Python str.isdigit(): every rune is a digit.
func isEntirelyNumeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

var (
	commonOnce sync.Once
	commonSet  map[string]bool
)

// commonPasswords loads Django's shipped list (data/common-passwords.txt,
// copied verbatim from django/contrib/auth at the pin — Royce Williams'
// ~20k-entry list; provenance recorded here since the file is framework
// data, not an upstream-backend file).
func commonPasswords() map[string]bool {
	commonOnce.Do(func() {
		commonSet = map[string]bool{}
		for _, line := range strings.Split(string(commonPasswordsData), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line != "" {
				commonSet[strings.ToLower(line)] = true
			}
		}
	})
	return commonSet
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}
