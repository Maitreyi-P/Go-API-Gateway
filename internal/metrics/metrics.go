// Package metrics defines the gateway's Prometheus collectors and small
// helpers for recording against them. Collectors are registered on an
// explicitly-provided prometheus.Registerer (never the global default
// registerer), so each Router/test can own an isolated, inspectable
// registry.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/breaker"
)

const (
	// NoBackend is the backend label value used when a request never
	// reached any backend (no route matched, it was rate-limited, or
	// every backend's breaker was Open).
	NoBackend = "none"
	// UnmatchedRoute is the route label value used when no configured
	// route matched the request path.
	UnmatchedRoute = "unmatched"
)

// Metrics holds the gateway's Prometheus collectors.
type Metrics struct {
	RequestsTotal       *prometheus.CounterVec
	RequestDuration     *prometheus.HistogramVec
	RateLimitRejections *prometheus.CounterVec
	CircuitBreakerState *prometheus.GaugeVec
}

// New creates the gateway's collectors and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total number of requests handled by the gateway, by route, backend, and response status code.",
		}, []string{"route", "backend", "status"}),

		RequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gateway_request_duration_seconds",
			Help: "Latency of requests handled by the gateway, by route and backend.",
			// Sub-millisecond to a few seconds: appropriate for a fast
			// local reverse proxy, with enough low-end resolution to
			// distinguish backend-call overhead from gateway overhead.
			Buckets: []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}, []string{"route", "backend"}),

		RateLimitRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limit_rejections_total",
			Help: "Total number of requests rejected by the rate limiter, by route.",
		}, []string{"route"}),

		CircuitBreakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "gateway_circuit_breaker_state",
			Help: "Current circuit breaker state per backend (0 = closed, 1 = open, 2 = half-open).",
		}, []string{"backend"}),
	}

	reg.MustRegister(m.RequestsTotal, m.RequestDuration, m.RateLimitRejections, m.CircuitBreakerState)

	return m
}

// ObserveRequest records one completed request: it increments
// RequestsTotal and observes RequestDuration for the given route, backend,
// and final status code.
func (m *Metrics) ObserveRequest(route, backendURL string, status int, duration time.Duration) {
	m.RequestsTotal.WithLabelValues(route, backendURL, strconv.Itoa(status)).Inc()
	m.RequestDuration.WithLabelValues(route, backendURL).Observe(duration.Seconds())
}

// RecordRateLimitRejection increments RateLimitRejections for route.
func (m *Metrics) RecordRateLimitRejection(route string) {
	m.RateLimitRejections.WithLabelValues(route).Inc()
}

// SetBreakerState records a backend's current circuit breaker state.
func (m *Metrics) SetBreakerState(backendURL string, state breaker.State) {
	m.CircuitBreakerState.WithLabelValues(backendURL).Set(breakerStateValue(state))
}

func breakerStateValue(s breaker.State) float64 {
	switch s {
	case breaker.Closed:
		return 0
	case breaker.Open:
		return 1
	case breaker.HalfOpen:
		return 2
	default:
		return -1
	}
}
