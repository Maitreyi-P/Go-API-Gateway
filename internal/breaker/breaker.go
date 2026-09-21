// Package breaker implements a hand-rolled, per-backend circuit breaker
// state machine (Closed -> Open -> HalfOpen -> ...), protecting the gateway
// from repeatedly hammering a backend that is failing.
package breaker

import (
	"sync"
	"time"
)

// State is one of the three circuit breaker states.
type State int

const (
	// Closed is the normal state: calls are allowed through and their
	// outcomes are tracked in a rolling window.
	Closed State = iota
	// Open rejects every call immediately, without attempting it, until
	// the configured cooldown has elapsed.
	Open
	// HalfOpen allows exactly one trial call through to test whether the
	// backend has recovered.
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// Clock abstracts the current time so tests can drive the breaker with a
// fake clock instead of sleeping in real time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Config configures a Breaker's tripping and recovery behavior.
type Config struct {
	// FailureThreshold is the failure rate (0, 1] within Window that trips
	// the breaker from Closed to Open, e.g. 0.5 for 50%.
	FailureThreshold float64
	// Window is the rolling time window over which the failure rate is
	// measured.
	Window time.Duration
	// Cooldown is how long the breaker stays Open before allowing a single
	// HalfOpen trial call.
	Cooldown time.Duration
}

type outcome struct {
	at      time.Time
	success bool
}

// Breaker is a circuit breaker for a single backend. It is safe for
// concurrent use.
//
// Usage: call Allow() before attempting a call. If it returns false, the
// call must not be attempted. If it returns true, the caller must attempt
// the call and report its result via Report(success) exactly once.
type Breaker struct {
	cfg   Config
	clock Clock

	mu    sync.Mutex
	state State

	// outcomes is the rolling window of recent Closed-state results,
	// oldest first. It is reset whenever the breaker trips or recovers.
	outcomes []outcome

	// openedAt is when the breaker last transitioned into Open.
	openedAt time.Time

	// halfOpenTrial is true while a single HalfOpen trial call is in
	// flight, so concurrent callers don't each get their own trial.
	halfOpenTrial bool
}

// New creates a Breaker using the real wall clock.
func New(cfg Config) *Breaker {
	return NewWithClock(cfg, realClock{})
}

// NewWithClock is New with an injectable clock, for deterministic tests.
func NewWithClock(cfg Config, clock Clock) *Breaker {
	return &Breaker{
		cfg:   cfg,
		clock: clock,
		state: Closed,
	}
}

// State reports the breaker's current state.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Allow reports whether a call should be attempted right now:
//   - Closed: always true.
//   - Open: false until Cooldown has elapsed since tripping, at which
//     point it transitions to HalfOpen and admits exactly one trial call
//     (returning true for that one caller only).
//   - HalfOpen: false for every caller except the single admitted trial.
//
// The caller must call Report exactly once for every Allow that returns
// true.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true

	case Open:
		if b.clock.Now().Sub(b.openedAt) < b.cfg.Cooldown {
			return false
		}
		// Cooldown elapsed: move to HalfOpen and admit one trial call.
		b.state = HalfOpen
		b.halfOpenTrial = true
		return true

	case HalfOpen:
		// A trial is already in flight; everyone else waits for it to
		// resolve.
		return false

	default:
		return false
	}
}

// Report records the outcome of a call that a prior Allow() admitted.
func (b *Breaker) Report(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.clock.Now()

	switch b.state {
	case HalfOpen:
		b.halfOpenTrial = false
		if success {
			b.state = Closed
			b.outcomes = nil
		} else {
			b.state = Open
			b.openedAt = now
			b.outcomes = nil
		}

	case Closed:
		b.outcomes = append(b.outcomes, outcome{at: now, success: success})
		b.outcomes = pruneOlderThan(b.outcomes, now, b.cfg.Window)

		if b.failureRate() >= b.cfg.FailureThreshold {
			b.state = Open
			b.openedAt = now
			b.outcomes = nil
		}

	case Open:
		// Allow() rejects every call while Open, so a well-behaved caller
		// never reports here. Ignore defensively.
	}
}

// failureRate returns the fraction of failures among the current window of
// outcomes. Callers must hold b.mu.
func (b *Breaker) failureRate() float64 {
	if len(b.outcomes) == 0 {
		return 0
	}
	failures := 0
	for _, o := range b.outcomes {
		if !o.success {
			failures++
		}
	}
	return float64(failures) / float64(len(b.outcomes))
}

// pruneOlderThan drops outcomes older than window relative to now,
// assuming outcomes is ordered oldest-first.
func pruneOlderThan(outcomes []outcome, now time.Time, window time.Duration) []outcome {
	cutoff := now.Add(-window)
	i := 0
	for i < len(outcomes) && outcomes[i].at.Before(cutoff) {
		i++
	}
	return outcomes[i:]
}
