// Per-connection and per-account live limits (apps/collab/limits.py).
//
// The upstream cache (locmem/redis) is shared across processes; the Go
// server is single-process, so the account windows live in an in-process
// map — same visible behaviour for the paired scenarios, recorded in
// PLAN.md as a substitution of the storage only.

// Ports backend/apps/collab/limits.py
package doory

import (
	"sync"
	"time"
)

// TokenBucket is limits.TokenBucket: two buckets, one counting messages and
// one counting bytes; a frame must fit in both.
type TokenBucket struct {
	rates    [2]float64
	capacity [2]float64
	tokens   [2]float64
	clock    func() float64
	stamp    float64
}

// NewTokenBucket ports TokenBucket.from_settings.
func NewTokenBucket() *TokenBucket {
	return newTokenBucketWith(
		envFloat("REALTIME_RATE_MESSAGES_PER_SECOND", 100),
		envFloat("REALTIME_RATE_BURST", 300),
		envFloat("REALTIME_RATE_BYTES_PER_SECOND", 1024*1024),
		envFloat("REALTIME_RATE_BYTES_BURST", 4*1024*1024),
		func() float64 { return float64(time.Now().UnixNano()) / 1e9 }, // seconds, like time.monotonic
	)
}

func newTokenBucketWith(messagesPerSec, messageBurst, bytesPerSec, byteBurst float64,
	clock func() float64) *TokenBucket {
	return &TokenBucket{
		rates:    [2]float64{messagesPerSec, bytesPerSec},
		capacity: [2]float64{messageBurst, byteBurst},
		tokens:   [2]float64{messageBurst, byteBurst},
		clock:    clock,
		stamp:    clock(),
	}
}

// Allow takes one message of `size` bytes; false (and nothing taken) when
// either bucket is short.
func (b *TokenBucket) Allow(size int) bool {
	now := b.clock()
	elapsed := now - b.stamp
	b.stamp = now
	for i := 0; i < 2; i++ {
		b.tokens[i] += elapsed * b.rates[i]
		if b.tokens[i] > b.capacity[i] {
			b.tokens[i] = b.capacity[i]
		}
	}
	if b.tokens[0] < 1 || b.tokens[1] < float64(size) {
		return false
	}
	b.tokens[0] -= 1
	b.tokens[1] -= float64(size)
	return true
}

// --------------------------------------------------- account windows -------

var (
	collabMu      sync.Mutex
	collabWindows = map[string]*collabWindow{}
)

type collabWindow struct {
	blockedUntil time.Time
	count        int
	startedAt    time.Time
}

// BlockUser ports limits.block_user: refuse the account's new live
// connections for REALTIME_BLOCK_SECONDS.
func BlockUser(userID string) {
	collabMu.Lock()
	defer collabMu.Unlock()
	collabWindows[userID] = &collabWindow{
		blockedUntil: time.Now().Add(time.Duration(envFloat("REALTIME_BLOCK_SECONDS", 600)) * time.Second),
	}
}

// AllowConnectionAttempt ports limits.allow_connection_attempt: at most
// REALTIME_CONNECTS_PER_MINUTE new connections per account and minute. An
// account that sent a slow-to-apply update (block_user) gets none for a while.
func AllowConnectionAttempt(userID string) bool {
	collabMu.Lock()
	defer collabMu.Unlock()
	now := time.Now()
	w := collabWindows[userID]
	if w != nil && now.Before(w.blockedUntil) {
		return false
	}
	limit := envFloat("REALTIME_CONNECTS_PER_MINUTE", 60)
	if w == nil || now.Sub(w.startedAt) > time.Minute {
		collabWindows[userID] = &collabWindow{startedAt: now, count: 1}
		return true
	}
	w.count++
	return float64(w.count) <= limit
}
