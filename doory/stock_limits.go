// Stock-provider pacing: a small per-key fixed window and provider pauses
// (stocklimits.py). The single binary has one process, so an in-memory map is
// enough; a provider asking us to back off is honoured for its whole key.
package doory

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const stockMaxPauseSeconds = 31 * 24 * 60 * 60

var stockRates = map[string]string{"freesound": "60/m", "pexels_video": "200/h"}

type stockLimitWindow struct {
	start time.Time
	count int
}

var (
	stockLimitsMu sync.Mutex
	stockWindows  = map[string]*stockLimitWindow{}
	stockPauses   = map[string]time.Time{}
)

func stockParseRate(rate string) (int, time.Duration, bool) {
	parts := strings.SplitN(strings.TrimSpace(rate), "/", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || n < 0 {
		return 0, 0, false
	}
	period := strings.ToLower(strings.TrimSpace(parts[1]))
	if len(period) > 1 && period[0] >= '0' && period[0] <= '9' { // 30d
		digits := 0
		for digits < len(period) && period[digits] >= '0' && period[digits] <= '9' {
			digits++
		}
		mult, _ := strconv.Atoi(period[:digits])
		if mult < 1 {
			mult = 1
		}
		period = period[digits:]
		switch period {
		case "s":
			return n, time.Duration(mult) * time.Second, true
		case "m":
			return n, time.Duration(mult) * time.Minute, true
		case "h":
			return n, time.Duration(mult) * time.Hour, true
		case "d":
			return n, time.Duration(mult) * 24 * time.Hour, true
		}
	}
	switch period {
	case "s", "sec", "second":
		return n, time.Second, true
	case "m", "min", "minute":
		return n, time.Minute, true
	case "h", "hour":
		return n, time.Hour, true
	case "d", "day":
		return n, 24 * time.Hour, true
	}
	return 0, 0, false
}

func stockAcquire(name string) float64 {
	stockLimitsMu.Lock()
	defer stockLimitsMu.Unlock()
	now := time.Now()
	if until, ok := stockPauses[name]; ok && until.After(now) {
		return until.Sub(now).Seconds() + 0.999
	}
	limit, period, ok := stockParseRate(stockRates[name])
	if !ok {
		return 0
	}
	window := int(now.UnixNano() / int64(period))
	key := fmt.Sprintf("%s:%d:%d", name, period, window)
	bucket := stockWindows[key]
	if bucket == nil {
		bucket = &stockLimitWindow{start: now}
		stockWindows[key] = bucket
	}
	if bucket.count >= limit {
		return period.Seconds() - now.Sub(bucket.start).Seconds()
	}
	bucket.count++
	return 0
}

func stockPause(name string, seconds float64) {
	if seconds < 1 {
		seconds = 1
	}
	if seconds > stockMaxPauseSeconds {
		seconds = stockMaxPauseSeconds
	}
	stockLimitsMu.Lock()
	defer stockLimitsMu.Unlock()
	stockPauses[name] = time.Now().Add(time.Duration(seconds * float64(time.Second)))
}
