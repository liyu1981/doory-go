// ai run support: the connection (catalog.connection), the source cap
// (read_source), the doory style build, quotas, the AICall log and the
// in-flight guard.
package doory

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// aiConn is styles.Connection for one call.
type aiConn struct {
	ref           string
	name          string
	baseURL       string
	style         string
	authHeader    string
	authValue     string
	timeout       time.Duration
	maxImageBytes int64
}

// aiConnection ports catalog.connection (style config + secret + limits).
func (a *App) aiConnection(option *aiOption) (aiConn, string, string) {
	var style, baseURL, name, authHeader, ref, secret string
	var timeoutSec int
	var maxImage int64
	isOperator := false
	var cfg any
	if option.env != nil {
		p := option.env
		style, baseURL, name, authHeader, ref = p.style, p.baseURL, p.name, p.authHeader, p.id
		secret = p.authValue
		timeoutSec = p.timeoutSeconds
		maxImage = p.maxImageBytes
		isOperator = true
		cfg = p.styleConfig
	} else if option.provider != nil {
		p := option.provider
		style, baseURL, name, authHeader, ref = p.style, p.baseURL, p.name, p.authHeader, p.id
		timeoutSec = p.timeout
		maxImage = p.maxImage
		isOperator = p.isOperator()
		cfg = p.styleConfigMap()
		if len(p.secret) > 0 {
			pt, err := a.Settings.aiDecrypt(p.secret)
			if err != nil {
				return aiConn{}, "secret_unreadable",
					"The provider's key can no longer be read; enter it again."
			}
			secret = pt
		}
	}
	if msg := aiStyleConfigError(style, cfg); msg != nil {
		return aiConn{}, "bad_response", "The provider is not configured correctly."
	}
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	if !isOperator && timeoutSec > 60 { // USER_PROVIDER_MAX_TIMEOUT
		timeoutSec = 60
	}
	if maxImage == 0 || maxImage > a.Settings.AIMaxImageBytes() {
		maxImage = a.Settings.AIMaxImageBytes()
	}
	return aiConn{
		ref: ref, name: name, baseURL: strings.TrimSuffix(baseURL, "/"), style: style,
		authHeader: authHeader, authValue: secret,
		timeout: time.Duration(timeoutSec) * time.Second, maxImageBytes: maxImage,
	}, "", ""
}

// readAISource ports services.read_source's cap: the stored file under the
// option's image limit.
func (a *App) readAISource(assetID string, maxBytes int64) ([]byte, error) {
	var path string
	if err := a.DB.QueryRow(`SELECT file FROM assets_asset WHERE id = ?`,
		hexUUID(assetID)).Scan(&path); err != nil {
		return nil, err
	}
	full := filepath.Join(a.Settings.MediaRoot, filepath.FromSlash(path))
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("too large")
	}
	return data, nil
}

// aiBuildDooryRequest ports DooryStyle.build for the doory style: multipart
// POST {base}/v1/{task} with the image, optional model and options fields.
// (In the paired scenarios the dial fails before any bytes are written; the
// shape follows styles/base multipart body construction.)
func aiBuildDooryRequest(conn aiConn, task, model string, image []byte) ([]byte, string, error) {
	boundary := "dooryAIBoundary0123456789abcdef"
	var b bytes.Buffer
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	fmt.Fprintf(&b, "Content-Disposition: form-data; name=\"image\"; filename=\"image.png\"\r\n")
	fmt.Fprintf(&b, "Content-Type: image/png\r\n\r\n")
	b.Write(image)
	b.WriteString("\r\n")
	if model != "" {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		fmt.Fprintf(&b, "Content-Disposition: form-data; name=\"model\"\r\n\r\n")
		b.WriteString(model)
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), "multipart/form-data; boundary=" + boundary, nil
}

// aiUsedToday ports quotas.used_today (COUNTED statuses, local midnight).
func (a *App) aiUsedToday(userID, providerRef string) int {
	var n int
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Format("2006-01-02T15:04:05.000000Z")
	_ = a.DB.QueryRow(`SELECT COUNT(*) FROM ai_aicall WHERE user_id = ? AND provider_ref = ?
	      AND created_at >= ? AND status IN ('ok','error','timeout')`,
		userID, providerRef, midnight).Scan(&n)
	return n
}

// aiSecondsUntilReset ports quotas.seconds_until_reset.
func (a *App) aiSecondsUntilReset() int {
	now := time.Now().UTC()
	next := now.Truncate(24 * time.Hour).Add(24 * time.Hour)
	secs := int(mathCeil(next.Sub(now).Seconds()))
	if secs < 1 {
		secs = 1
	}
	return secs
}

func mathCeil(v float64) float64 {
	i := float64(int64(v))
	if i < v {
		return i + 1
	}
	return i
}

// writeThrottledQuota renders the Throttled envelope: DRF joins the default
// detail with extra_detail (the singular/plural attrs on QuotaReached are
// unused in the REST path - core's handler only adds retry_after).
func writeThrottledQuota(w http.ResponseWriter, wait int) {
	detail := fmt.Sprintf(
		"You have used all of today's calls of this option. Pick another option or try again tomorrow. Expected available in %d seconds.",
		wait)
	w.Header().Set("Retry-After", strconv.Itoa(wait))
	w.Header().Set("Content-Type", "application/json")
	body := fmt.Sprintf(`{"error":{"code":"ai_quota","message":%s,"retry_after":%d}}`,
		jsonStr(detail), wait)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte(body))
}

// logAICall ports services.log_call: one row per attempt (no image data, no
// secrets).
func (a *App) logAICall(u *UserRow, option *aiOption, task, model, status, errCode string,
	durationMs, bytesIn, bytesOut int,
) {
	var providerID any
	if option.provider != nil {
		providerID = option.provider.id
	}
	now := time.Now().UTC()
	_, _ = a.DB.Exec(`INSERT INTO ai_aicall
	      (id, created_at, updated_at, provider_ref, provider_name, model, task, status,
	       error_code, duration_ms, bytes_in, bytes_out, user_id, provider_id)
	      VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newUUID4HexDashed(), formatDjangoDateTime(now), formatDjangoDateTime(now),
		option.providerID, option.providerName, model, task, status, errCode,
		durationMs, bytesIn, bytesOut, u.ID, providerID)
}

func msSince(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// ------------------------------------------------------------ in-flight -----

var aiInFlight sync.Map // userID -> *int

const aiMaxInFlightPerUser = 1

func (a *App) aiEnterInFlight(userID string) bool {
	v, _ := aiInFlight.LoadOrStore(userID, new(int))
	n := v.(*int)
	if *n >= aiMaxInFlightPerUser {
		return false
	}
	*n++
	return true
}

func (a *App) aiExitInFlight(userID string) {
	if v, ok := aiInFlight.Load(userID); ok {
		if n := v.(*int); *n > 0 {
			*n--
		}
	}
}
