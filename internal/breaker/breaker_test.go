package breaker_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/breaker"
)

// fakeClock is an injectable, manually-advanced Clock so tests can exercise
// cooldown/window behavior without sleeping in real time.
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

type stepKind int

const (
	kindAdvance stepKind = iota
	kindAllow
	kindReport
)

// step is one action against a single Breaker within a test case.
type step struct {
	kind        stepKind
	advance     time.Duration
	success     bool // used when kind == kindReport
	wantAllowed bool // used when kind == kindAllow
}

func advanceStep(d time.Duration) step { return step{kind: kindAdvance, advance: d} }
func allowStep(want bool) step         { return step{kind: kindAllow, wantAllowed: want} }
func reportStep(success bool) step     { return step{kind: kindReport, success: success} }

func TestBreaker_Transitions(t *testing.T) {
	cfg := breaker.Config{
		FailureThreshold: 0.5,
		Window:           10 * time.Second,
		Cooldown:         15 * time.Second,
	}

	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "closed stays closed under acceptable failure rate",
			steps: []step{
				reportStep(true),
				reportStep(true),
				reportStep(false), // 1/3 ≈ 33% < 50%
				allowStep(true),
			},
		},
		{
			name: "closed trips to open on threshold breach",
			steps: []step{
				reportStep(true),
				reportStep(false),
				reportStep(false), // 2/3 ≈ 67% >= 50%
				allowStep(false),  // now open, rejected without a real call
			},
		},
		{
			name: "open rejects immediately and keeps rejecting within the cooldown window",
			steps: []step{
				reportStep(false),
				reportStep(false), // 2/2 = 100% -> trips
				allowStep(false),
				advanceStep(5 * time.Second), // still well within the 15s cooldown
				allowStep(false),
				advanceStep(9 * time.Second), // 14s elapsed total, still short of cooldown
				allowStep(false),
			},
		},
		{
			name: "open transitions to half-open and admits exactly one trial after cooldown",
			steps: []step{
				reportStep(false),
				reportStep(false), // trips
				advanceStep(15 * time.Second),
				allowStep(true),  // the one trial call
				allowStep(false), // a concurrent caller is rejected until the trial resolves
			},
		},
		{
			name: "half-open success closes the circuit and resets counters",
			steps: []step{
				reportStep(false),
				reportStep(false), // trips
				advanceStep(15 * time.Second),
				allowStep(true),  // trial admitted
				reportStep(true), // trial succeeds
				allowStep(true),  // closed again, freely allowed
				allowStep(true),
			},
		},
		{
			name: "half-open failure reopens the circuit and restarts the cooldown",
			steps: []step{
				reportStep(false),
				reportStep(false), // trips
				advanceStep(15 * time.Second),
				allowStep(true),   // trial admitted
				reportStep(false), // trial fails
				allowStep(false),  // back to open, immediately rejected
				advanceStep(10 * time.Second),
				allowStep(false), // only 10s into the fresh 15s cooldown
				advanceStep(5 * time.Second),
				allowStep(true), // fresh 15s cooldown has now elapsed; new trial admitted
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeClock(time.Unix(0, 0))
			b := breaker.NewWithClock(cfg, clock)

			for i, s := range tc.steps {
				if s.advance > 0 {
					clock.Advance(s.advance)
				}
				switch s.kind {
				case kindAllow:
					got := b.Allow()
					if got != s.wantAllowed {
						t.Errorf("step %d: Allow() = %v, want %v (state=%s)", i, got, s.wantAllowed, b.State())
					}
				case kindReport:
					b.Report(s.success)
				}
			}
		})
	}
}

func TestBreaker_State(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	b := breaker.NewWithClock(breaker.Config{
		FailureThreshold: 0.5,
		Window:           10 * time.Second,
		Cooldown:         15 * time.Second,
	}, clock)

	if got := b.State(); got != breaker.Closed {
		t.Fatalf("initial state = %s, want %s", got, breaker.Closed)
	}

	b.Report(false)
	b.Report(false)
	if got := b.State(); got != breaker.Open {
		t.Fatalf("state after threshold breach = %s, want %s", got, breaker.Open)
	}

	clock.Advance(15 * time.Second)
	if !b.Allow() {
		t.Fatal("expected the trial call to be admitted after cooldown")
	}
	if got := b.State(); got != breaker.HalfOpen {
		t.Fatalf("state after cooldown elapses = %s, want %s", got, breaker.HalfOpen)
	}

	b.Report(true)
	if got := b.State(); got != breaker.Closed {
		t.Fatalf("state after a successful trial = %s, want %s", got, breaker.Closed)
	}
}

func TestBreaker_WindowExpiresOldOutcomes(t *testing.T) {
	clock := newFakeClock(time.Unix(0, 0))
	b := breaker.NewWithClock(breaker.Config{
		FailureThreshold: 0.5,
		Window:           10 * time.Second,
		Cooldown:         15 * time.Second,
	}, clock)

	// A long history of successes, old enough to fall outside the window
	// by the time we check again.
	for i := 0; i < 10; i++ {
		b.Report(true)
	}

	clock.Advance(11 * time.Second)

	// A single fresh failure. If the old successes still counted, the
	// diluted rate (1/11 ≈ 9%) would stay well under the 50% threshold
	// and the backend's current 100% failure rate would go unnoticed.
	// Once they've aged out of the window, this fresh failure alone
	// should trip it.
	b.Report(false)

	if b.Allow() {
		t.Fatal("expected the breaker to trip once stale successes fell out of the window, leaving only the fresh failure")
	}
}
