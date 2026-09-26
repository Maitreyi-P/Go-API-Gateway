// Package proxy implements the gateway's routing and reverse-proxy core:
package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/breaker"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/metrics"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/ratelimit"
)


type Router struct {
	routes  []*compiledRoute
	metrics *metrics.Metrics
	logger  *slog.Logger
}


type backendTarget struct {
	url     *url.URL
	proxy   *httputil.ReverseProxy
	breaker *breaker.Breaker 
}


type compiledRoute struct {
	prefix   string
	backends []*backendTarget
	next     atomic.Uint32


	limiter *ratelimit.Limiter
}


type RouteInfo struct {
	PathPrefix string
	Backends   []string
}


func NewRouter(cfg *config.Config, logger *slog.Logger, m *metrics.Metrics) (*Router, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if m == nil {
		m = metrics.New(prometheus.NewRegistry())
	}

	routes := make([]*compiledRoute, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		var breakerCfg *breaker.Config
		if cb := r.CircuitBreaker; cb != nil {
			breakerCfg = &breaker.Config{
				FailureThreshold: cb.FailureThreshold,
				Window:           time.Duration(cb.WindowSeconds) * time.Second,
				Cooldown:         time.Duration(cb.CooldownSeconds) * time.Second,
			}
		}

		backends := make([]*backendTarget, 0, len(r.Backends))
		for _, b := range r.Backends {
			u, err := url.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("route %s: parsing backend %q: %w", r.PathPrefix, b, err)
			}
			bt := &backendTarget{
				url:   u,
				proxy: newReverseProxy(u, logger),
			}
			if breakerCfg != nil {
				bt.breaker = breaker.New(*breakerCfg)
				m.SetBreakerState(bt.url.String(), bt.breaker.State())
			}
			backends = append(backends, bt)
		}

		cr := &compiledRoute{
			prefix:   r.PathPrefix,
			backends: backends,
		}
		if rl := r.RateLimit; rl != nil {
			cr.limiter = ratelimit.NewLimiter(rl.RequestsPerSecond, rl.Burst)
		}
		routes = append(routes, cr)
	}

	sort.Slice(routes, func(i, j int) bool {
		return len(routes[i].prefix) > len(routes[j].prefix)
	})

	return &Router{routes: routes, metrics: m, logger: logger}, nil
}


func (rt *Router) Routes() []RouteInfo {
	out := make([]RouteInfo, 0, len(rt.routes))
	for _, r := range rt.routes {
		backends := make([]string, len(r.backends))
		for i, bt := range r.backends {
			backends[i] = bt.url.String()
		}
		out = append(out, RouteInfo{PathPrefix: r.prefix, Backends: backends})
	}
	return out
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	route := rt.match(r.URL.Path)
	if route == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no route matches path %q", r.URL.Path))
		rt.finish(r, metrics.UnmatchedRoute, metrics.NoBackend, http.StatusNotFound, start)
		return
	}

	if !route.allowRequest(w, r, rt.metrics, rt.logger) {
		rt.finish(r, route.prefix, metrics.NoBackend, http.StatusTooManyRequests, start)
		return
	}

	backendURL, status, ok := route.forward(w, r, rt.metrics, rt.logger)
	if !ok {
		writeJSONError(w, http.StatusServiceUnavailable, "circuit open: all backends for this route are currently unavailable")
		status = http.StatusServiceUnavailable
		backendURL = metrics.NoBackend
	}
	rt.finish(r, route.prefix, backendURL, status, start)
}


func (rt *Router) finish(r *http.Request, route, backendURL string, status int, start time.Time) {
	duration := time.Since(start)
	rt.metrics.ObserveRequest(route, backendURL, status, duration)
	rt.logger.Info("request completed",
		"method", r.Method,
		"path", r.URL.Path,
		"route", route,
		"backend", backendURL,
		"status", status,
		"latency_ms", duration.Milliseconds(),
	)
}

func (rt *Router) match(path string) *compiledRoute {
	for _, r := range rt.routes {
		if strings.HasPrefix(path, r.prefix) {
			return r
		}
	}
	return nil
}


func (cr *compiledRoute) allowRequest(w http.ResponseWriter, r *http.Request, m *metrics.Metrics, logger *slog.Logger) bool {
	if cr.limiter == nil {
		return true
	}

	key := ratelimit.ClientIP(r)
	allowed, retryAfter := cr.limiter.Allow(key)
	if allowed {
		return true
	}

	m.RecordRateLimitRejection(cr.prefix)
	logger.Warn("rate limit exceeded",
		"route", cr.prefix,
		"client", key,
		"path", r.URL.Path,
		"retry_after_ms", retryAfter.Milliseconds(),
	)

	retryAfterSeconds := int(math.Ceil(retryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
	return false
}


func (cr *compiledRoute) forward(w http.ResponseWriter, r *http.Request, m *metrics.Metrics, logger *slog.Logger) (backendURL string, status int, ok bool) {
	n := len(cr.backends)
	start := int(cr.next.Add(1) % uint32(n))

	for i := 0; i < n; i++ {
		bt := cr.backends[(start+i)%n]

		if bt.breaker != nil {
			before := bt.breaker.State()
			allowed := bt.breaker.Allow()
			recordBreakerState(m, logger, bt, before)
			if !allowed {
				continue
			}
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		bt.proxy.ServeHTTP(rec, r)

		if bt.breaker != nil {
			before := bt.breaker.State()
			bt.breaker.Report(rec.status < http.StatusInternalServerError)
			recordBreakerState(m, logger, bt, before)
		}
		return bt.url.String(), rec.status, true
	}

	return "", 0, false
}


func recordBreakerState(m *metrics.Metrics, logger *slog.Logger, bt *backendTarget, before breaker.State) {
	after := bt.breaker.State()
	m.SetBreakerState(bt.url.String(), after)
	if after != before {
		logger.Warn("circuit breaker state changed",
			"backend", bt.url.String(),
			"from", before.String(),
			"to", after.String(),
		)
	}
}


type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (rec *statusRecorder) WriteHeader(status int) {
	if !rec.wroteHeader {
		rec.status = status
		rec.wroteHeader = true
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if !rec.wroteHeader {
		rec.WriteHeader(http.StatusOK)
	}
	return rec.ResponseWriter.Write(b)
}


func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func newReverseProxy(target *url.URL, logger *slog.Logger) *httputil.ReverseProxy {
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
	}

	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		
		logger.Warn("backend unreachable",
			"backend", target.String(),
			"method", r.Method,
			"path", r.URL.Path,
			"error", err,
		)
		writeJSONError(w, http.StatusBadGateway, "backend unreachable")
	}

	return &httputil.ReverseProxy{
		Director:     director,
		ErrorHandler: errorHandler,
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
