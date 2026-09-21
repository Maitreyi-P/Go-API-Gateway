// Package ratelimit implements a hand-rolled, per-client token bucket rate
// limiter, plus a helper for deriving a client key from an HTTP request.
package ratelimit

import (
	"sync"
	"time"
)

// Clock abstracts the current time so tests can drive the limiter with a
// fake clock instead of sleeping in real time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// bucket holds one client's token-bucket state.
type bucket struct {
	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
}

// Limiter enforces a requests-per-second rate with a burst allowance,
// tracking one independent token bucket per client key. It is safe for
// concurrent use.
type Limiter struct {
	rate  float64 // tokens added per second
	burst float64 // maximum tokens a bucket can hold

	clock Clock

	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewLimiter creates a Limiter allowing requestsPerSecond sustained
// throughput with up to burst requests in a single instant, per client key.
func NewLimiter(requestsPerSecond float64, burst int) *Limiter {
	return NewLimiterWithClock(requestsPerSecond, burst, realClock{})
}

// NewLimiterWithClock is NewLimiter with an injectable clock, for
// deterministic tests.
func NewLimiterWithClock(requestsPerSecond float64, burst int, clock Clock) *Limiter {
	return &Limiter{
		rate:    requestsPerSecond,
		burst:   float64(burst),
		clock:   clock,
		buckets: make(map[string]*bucket),
	}
}

// Allow reports whether a request from key is permitted right now. If so,
// it atomically consumes one token. If not, it also returns an estimate of
// how long the caller should wait before the next token becomes available.
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	b := l.bucketFor(key)

	b.mu.Lock()
	defer b.mu.Unlock()

	now := l.clock.Now()
	if elapsed := now.Sub(b.lastRefill).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.lastRefill = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	deficit := 1 - b.tokens
	wait := time.Duration(deficit / l.rate * float64(time.Second))
	return false, wait
}

// bucketFor returns the bucket for key, creating a fresh, fully-topped-up
// one on first use.
func (l *Limiter) bucketFor(key string) *bucket {
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastRefill: l.clock.Now()}
		l.buckets[key] = b
	}
	return b
}
