package ratelimit_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/ratelimit"
)

// fakeClock is an injectable, manually-advanced Clock so tests can exercise
// refill behavior without sleeping in real time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// step is one call to Limiter.Allow within a test case, optionally preceded
// by advancing the fake clock.
type step struct {
	key         string
	advance     time.Duration
	wantAllowed bool
}

func TestLimiter_Allow(t *testing.T) {
	tests := []struct {
		name  string
		rps   float64
		burst int
		steps []step
	}{
		{
			name:  "burst up to bucket size succeeds",
			rps:   1,
			burst: 3,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: true},
			},
		},
		{
			name:  "request beyond burst is rejected",
			rps:   1,
			burst: 2,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: false},
			},
		},
		{
			name:  "request succeeds again after waiting the refill time",
			rps:   1, // 1 token/sec
			burst: 1,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},                       // consumes the only token
				{key: "1.1.1.1", wantAllowed: false},                      // none left
				{key: "1.1.1.1", advance: time.Second, wantAllowed: true}, // fully refilled
			},
		},
		{
			name:  "partial refill is not enough for another request",
			rps:   1,
			burst: 1,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", advance: 500 * time.Millisecond, wantAllowed: false},
			},
		},
		{
			name:  "refill never exceeds burst capacity",
			rps:   1,
			burst: 2,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: true},
				// Wait far longer than needed to refill; bucket should cap
				// at burst (2), not accumulate unbounded credit.
				{key: "1.1.1.1", advance: 10 * time.Second, wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: false},
			},
		},
		{
			name:  "two client IPs have independent buckets",
			rps:   1,
			burst: 1,
			steps: []step{
				{key: "1.1.1.1", wantAllowed: true},
				{key: "1.1.1.1", wantAllowed: false}, // client A now throttled
				{key: "2.2.2.2", wantAllowed: true},  // client B unaffected
				{key: "2.2.2.2", wantAllowed: false}, // client B now throttled too
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock(time.Unix(0, 0))
			limiter := ratelimit.NewLimiterWithClock(tc.rps, tc.burst, clock)

			for i, s := range tc.steps {
				if s.advance > 0 {
					clock.Advance(s.advance)
				}
				allowed, _ := limiter.Allow(s.key)
				if allowed != s.wantAllowed {
					t.Errorf("step %d (key=%s): Allow() = %v, want %v", i, s.key, allowed, s.wantAllowed)
				}
			}
		})
	}
}

func TestLimiter_Allow_RetryAfterEstimate(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	limiter := ratelimit.NewLimiterWithClock(2, 1, clock) // 2 tokens/sec, burst 1

	if allowed, _ := limiter.Allow("k"); !allowed {
		t.Fatal("expected the first request to be allowed")
	}

	allowed, wait := limiter.Allow("k")
	if allowed {
		t.Fatal("expected the immediate second request to be rejected")
	}
	// At 2 tokens/sec, refilling from 0 to 1 token takes ~500ms.
	if wait <= 0 || wait > 600*time.Millisecond {
		t.Errorf("retryAfter = %v, want in (0, 600ms]", wait)
	}

	clock.Advance(wait)
	if allowed, _ := limiter.Allow("k"); !allowed {
		t.Error("expected request to be allowed after waiting the estimated retryAfter duration")
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		want       string
	}{
		{
			name:       "uses RemoteAddr host when no XFF header",
			remoteAddr: "203.0.113.5:54321",
			want:       "203.0.113.5",
		},
		{
			name:       "prefers first address in X-Forwarded-For",
			remoteAddr: "10.0.0.1:12345",
			xff:        "198.51.100.7, 10.0.0.1",
			want:       "198.51.100.7",
		},
		{
			name:       "trims whitespace around the XFF address",
			remoteAddr: "10.0.0.1:12345",
			xff:        "  198.51.100.7  ,10.0.0.1",
			want:       "198.51.100.7",
		},
		{
			name:       "falls back to raw RemoteAddr if it has no port",
			remoteAddr: "203.0.113.5",
			want:       "203.0.113.5",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}

			if got := ratelimit.ClientIP(req); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
