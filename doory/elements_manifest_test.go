package doory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

// TestPackFingerprintMatchesOracle pins the fingerprint text: the oracle's
// json.dumps(payload, sort_keys=True) hash for the generated packs.
func TestPackFingerprintMatchesOracle(t *testing.T) {
	manifest, err := loadElementManifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// Every pack's fingerprint (all 25), computed by the oracle's
	// json.dumps(payload, sort_keys=True) + sha256.
	want := map[string]string{
		"backgrounds-patterns": "c573001eae44343961d74688459c5fddfc084f42c6c782c6488da2d64eae7cc2",
		"bootstrap-fill":       "e0fd3ebec3e0667b23916e8b8b0ebb65034e5743d50e7fdec6b8e81aa466ea5e",
		"bootstrap-outline":    "ccadcdf06cbaae7305a225c93e3bae06b1135b6fd865b2541429e1e9d250742c",
		"charts":               "8a39e3ebf475956369ba0fbf006aa2e6604c0bcc1e7439a36b04a13d4cb0a652",
		"dividers-borders":     "ea36a17df4bce791f2c9c33de00367dbe7bb4157d5e6f713ddd784c44567be7a",
		"doodle-accents":       "8fe9518d38571d40b5b9d414bd6160045e3c24a84bab66f7edd7fc41298709a0",
		"fluent-emoji":         "1f57f660a34fc0ac446fa83164c038e166c09f79c5b6323f470a0b3e0f79130d",
		"frames-badges":        "2b2aec384c3688932e2db291046b2a054cc32f0f7d5860b2f4b90d857c2f525e",
		"heroicons-outline":    "d5510159bdbab8d8e47db4f2ee35767bf9d2ba24fde2a360cddf9f847d9e2b64",
		"heroicons-solid":      "67b11f0163dd7aabc5153461c47b3450d10b15f5a73d902008099cec5b95af2b",
		"lucide":               "48809f6533c93361681248e6eda9243d503f4c4a5d092ffd178d86bb9d970dc2",
		"material-filled":      "14702c1a1f3c56c8b1f926e35eacb2594c8ff830b3fba8c7f9e1adab74b096fb",
		"material-outlined":    "1a93a5116e8bf546536b8bfb64542315a167cd810ec0104c837bab31278d05a7",
		"noto-emoji":           "dc2237b3cf23e7b100e9776c66fe0fd0c883d98616a7139717c0e7d4ffbe9470",
		"open-doodles":         "492c21293a27a7c0c9a3895e4d1f8806a8eba3689ed95ddabb25c71c1a6d6966",
		"open-peeps":           "0cf8f68cb99ec275b5e24533b9b493798af44dfdda7331194a01853003dd42d7",
		"phosphor-bold":        "7865362ccb7439a576019e163192c892d77860d6bfe76d3401344b6f354e1118",
		"phosphor-duotone":     "602a6bd898cc72eec1b7efd4776477790a4f7957c3749389b3f19a0518766819",
		"phosphor-fill":        "8e8e9f68b0bd982e086caff932a9d18a819c2954ccd984af5027c075bdeef67c",
		"phosphor-regular":     "423982e582fcd6ed2309870f1897d236fbcf29b94ae7a0710e8f6b4553de0116",
		"sale-promo":           "4d9265fbdd358dc56f21e679164e896c5775615c6a7f9f460a56f306b03f989f",
		"social-media":         "b36ddd8eb5923cd746c9c73f1e313778967a6b1c75dd2f0be56e274c0d57c793",
		"speech-bubbles":       "7fe88b8e86707cfc92b6942e196d67e3a9dd4c56231e86b5c07cbf23151e2740",
		"tabler-filled":        "c898a9ce263c93acdbdd452401bd8c7a59b40516132b67a3a235fb13f9e55869",
		"tabler-outline":       "1f0c67305b5e3a85fc955a5db249d771ee15163fbfcecdc07d25c8092259d0b3",
	}
	for _, pack := range manifest.Packs {
		expected, ok := want[pack.ID]
		if !ok {
			continue
		}
		_ = sha256.Sum256
		if os.Getenv("FINGERPRINT_DUMP") != "" {
			fmt.Println(packFingerprintText)
		}
		if pack.Fingerprint != expected {
			t.Fatalf("%s fingerprint = %s, want %s", pack.ID, pack.Fingerprint, expected)
		}
	}
	_ = hex.EncodeToString
}
