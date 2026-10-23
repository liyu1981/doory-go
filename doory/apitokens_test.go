package doory

import "testing"

// PROBE-ANCHORED cross-implementation proofs from probes/0003-apitokens:
// the oracle created a token under DJANGO_SECRET_KEY=
// "parity-oracle-secret-key-0123456789" for a user whose password hash is
// recorded in user-password.txt; token-rows.txt carries the row. Go must
// reproduce BOTH digests byte-for-byte:
//   - secret_hash = sha256(plaintext)
//   - password_digest = salted_hmac("apitokens.password_state",
//     password_hash, secret=SECRET_KEY, algorithm=sha256)
//
// Mutation: change the HMAC key derivation (e.g. key_salt+secret hashing),
// the digest hexing, or the password-digest binding -> these fail while a
// Go-only round trip could still pass.
func TestProbedTokenDigests(t *testing.T) {
	const (
		plaintext   = "doory_43070cd98cb8_zfB63f374IgchQeZteM0NR5aXMZ1qgK59C1e7aCwwI8"
		secretHash  = "e41bae4468c12dfe70f4b6d074b0640466e6238d198fadbe0563522a71b76053"
		passwordHsh = "pbkdf2_sha256$1500000$EO0CPs0WY6lU4OSmDkSLE7$edQpj1KSlVaoyQd+AuqFj+0WKmhrZNlYMG9f/fGLgpM="
		secretKey   = "parity-oracle-secret-key-0123456789"
		wantDigest  = "3b2c76eb5bbdc7501bcf62e27d267585fd47e6dde55adcffb1ab0e7fba68646d"
	)
	if got := tokenDigest(plaintext); got != secretHash {
		t.Errorf("secret_hash:\n got %s\nwant %s", got, secretHash)
	}
	if got := passwordDigest(passwordHsh, secretKey); got != wantDigest {
		t.Errorf("password_digest:\n got %s\nwant %s", got, wantDigest)
	}
	// A different secret (fallback key) must produce a different digest.
	if passwordDigest(passwordHsh, "other-key") == wantDigest {
		t.Error("fallback-key digest collided with the primary key's")
	}
}

// Issue format: plaintext = doory_<12 hex>_<43 urlsafe-b64> — the scenario
// comparator asserts this against the oracle's own token bytes; this unit
// pins the generator when no DB is around (mutation: wrong byte counts or
// padded base64 -> the regex shape fails).
func TestTokenPlaintextFormat(t *testing.T) {
	prefix := make([]byte, prefixBytes)
	secret := make([]byte, secretBytes)
	// deterministic fill (generator logic mirrors issueToken's encoding)
	for i := range prefix {
		prefix[i] = byte(i)
	}
	for i := range secret {
		secret[i] = byte(255 - i)
	}
	plaintext := tokenPrefix + "_" + hexENCODE(prefix) + "_" + b64url(secret)
	if !plaintextShape.MatchString(plaintext) {
		t.Errorf("issued token shape wrong: %q", plaintext)
	}
	if got := splitTokenPrefix(plaintext); got == "" || len(got) != 12 {
		t.Errorf("prefix split wrong: %q", got)
	}
}
