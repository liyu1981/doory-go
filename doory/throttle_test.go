package doory

import (
	"net/http"
	"testing"
)

func TestDefaultAnonThrottleAndHealthBypass(t *testing.T) {
	env := newAdminEnv(t)
	env.app.Settings.ThrottleRates["anon"] = "1/min"

	res, body := env.get("/api/v1/config/")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first config = %d: %s", res.StatusCode, body)
	}
	res, body = env.get("/api/v1/config/")
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("second config = %d Retry-After=%q: %s", res.StatusCode,
			res.Header.Get("Retry-After"), body)
	}

	// Health disables DRF throttles (@throttle_classes([])).
	env.app.Settings.ThrottleRates["anon"] = "0/min"
	for i := 0; i < 2; i++ {
		res, body = env.get("/api/v1/health/")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("health %d = %d: %s", i, res.StatusCode, body)
		}
	}
}

func TestAuthScopeReplacesDefaultThrottles(t *testing.T) {
	env := newAdminEnv(t)
	env.app.Settings.ThrottleRates["auth"] = "1/min"
	env.app.Settings.ThrottleRates["anon"] = "0/min"

	res, body := env.doJSON("/api/v1/auth/login/", `{"email":"nobody@example.com","password":"wrong"}`)
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("first login was already throttled: %s", body)
	}
	res, body = env.doJSON("/api/v1/auth/login/", `{"email":"nobody@example.com","password":"wrong"}`)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second login = %d, want 429: %s", res.StatusCode, body)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatalf("missing Retry-After")
	}
}

func TestScopedUploadThrottleOnAssetCreate(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("upload-throttle@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.ThrottleRates["user"] = "1000/min"
	env.app.Settings.ThrottleRates["upload"] = "1/min"

	res, body := env.doJSON("/api/v1/assets/", `{}`)
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("first upload was already throttled: %s", body)
	}
	res, body = env.doJSON("/api/v1/assets/", `{}`)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second upload = %d, want 429: %s", res.StatusCode, body)
	}
}

func TestScopedStockThrottleIsPerUser(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("stock-throttle@example.com", false, false)
	env.loginAs(user)
	env.app.Settings.ThrottleRates["user"] = "1000/min"
	env.app.Settings.ThrottleRates["stock"] = "0/min"

	res, body := env.get("/api/v1/stock/photos/")
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("stock search = %d, want 429: %s", res.StatusCode, body)
	}
	// A different user has their own stock bucket.
	other := env.makeUser("stock-other@example.com", false, false)
	env.app.Settings.ThrottleRates["stock"] = "1/min"
	env.loginAs(other)
	res, body = env.get("/api/v1/stock/photos/")
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatalf("another user inherited the stock limit: %s", body)
	}
}
