package aikeys

import (
	"strings"
	"testing"
)

// Fixtures encrypted by the oracle's own cryptography library (Fernet, HKDF-derived
// key from the parity DJANGO_SECRET_KEY) — read compatibility with every secret the
// oracle has ever stored.
const (
	paritySecretKey = "parity-oracle-secret-key-0123456789"
	oracleToken1    = "gAAAAABq2ho66eX3R9EoNiqaTCe6BzZhk-zXF8SH4-37S48pJ6vuQB4s-Vwq-VhFE9B3SIXxcCtLF5jvs2-8NdjifVIq49SuvSbskE4sCvQyqPT6d0Uv4To="
	oraclePlain1    = "sk-oracle-secret-1"
	oracleToken2    = "gAAAAABq2ho6X008Y_oH8WfIXjRBOMiF7NlHfILAuKfHudZdGB4UGQ5t_dTdFM_ZRrrt61WshWrL2Pu_Pdfl68j9Df7FoNtEaICh5S3Hoz7m_p0aW8wKSq0="
	oraclePlain2    = "Bearer cross-impl-token-2"
)

func TestDeriveKeyMatchesOracle(t *testing.T) {
	// Derived key as printed by cryptography's HKDF in the oracle venv.
	want := "scGegw_snpXu7udOavgONtY4XzCsfBccM0O9X2PxTd4="
	if got := string(DeriveKey(paritySecretKey)); got != want {
		t.Fatalf("derive key mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestDecryptOracleTokens(t *testing.T) {
	key := string(DeriveKey(paritySecretKey))
	for _, tc := range []struct{ token, want string }{
		{oracleToken1, oraclePlain1},
		{oracleToken2, oraclePlain2},
	} {
		got, err := Decrypt([]byte(tc.token), []string{key})
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if string(got) != tc.want {
			t.Fatalf("plaintext mismatch: got %q want %q", got, tc.want)
		}
	}
}

func TestRoundTripAndWrongKey(t *testing.T) {
	key := string(DeriveKey(paritySecretKey))
	tok, err := Encrypt("sk-roundtrip-1", key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	got, err := Decrypt([]byte(tok), []string{key})
	if err != nil || string(got) != "sk-roundtrip-1" {
		t.Fatalf("roundtrip: %q %v", got, err)
	}
	// Wrong key: HMAC fails → SecretUnreadable.
	if _, err := Decrypt([]byte(tok), []string{string(DeriveKey("other-secret"))}); err == nil {
		t.Fatal("wrong key must fail")
	}
	// Truncated/garbage tokens: unreadable, never a panic.
	for _, bad := range []string{"", "not-a-token", "gAAAAAB"} {
		if _, err := Decrypt([]byte(bad), []string{key}); err == nil {
			t.Fatalf("bad token %q must fail", bad)
		}
	}
	// Invalid primary key: error like secrets.py's SecretUnreadable("AI_SECRETS_KEY is not a valid Fernet key.").
	if _, _, err := keyMaterial("too-short"); err == nil || !strings.Contains(err.Error(), "32 url-safe") {
		t.Fatalf("invalid key message: %v", err)
	}
}
