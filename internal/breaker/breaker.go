
package breaker

import (
	"sync"
	"time"
)


type State int

const (
	
	Closed State = iota
	
	Open
	
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


type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }


type Config struct {

	FailureThreshold float64
	Window time.Duration
	Cooldown time.Duration
}

type outcome struct {
	at      time.Time
	success bool
}


type Breaker struct {
	cfg   Config
	clock Clock

	mu    sync.Mutex
	state State

	
	outcomes []outcome

	
	openedAt time.Time

	
	halfOpenTrial bool
}

// New creates a Breaker using the real wall clock.
func New(cfg Config) *Breaker {
	return NewWithClock(cfg, realClock{})
}


func NewWithClock(cfg Config, clock Clock) *Breaker {
	return &Breaker{
		cfg:   cfg,
		clock: clock,
		state: Closed,
	}
}


func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}


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

		b.state = HalfOpen
		b.halfOpenTrial = true
		return true

	case HalfOpen:
		return false

	default:
		return false
	}
}

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
	}
}


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


func pruneOlderThan(outcomes []outcome, now time.Time, window time.Duration) []outcome {
	cutoff := now.Add(-window)
	i := 0
	for i < len(outcomes) && outcomes[i].at.Before(cutoff) {
		i++
	}
	return outcomes[i:]
}
