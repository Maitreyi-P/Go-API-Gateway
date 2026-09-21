// Package proxy implements the gateway's routing and reverse-proxy core:
// matching requests to a configured route by longest path prefix,
// enforcing that route's rate limit (if any), then forwarding to a
// backend chosen by round-robin among those whose circuit breaker
// currently allows calls.
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

	"github.com/Maitreyi-P/Go-API-Gateway/internal/breaker"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/ratelimit"
)

// Router matches incoming requests to a route and proxies them to a
// backend. It implements http.Handler.
type Router struct {
	routes []*compiledRoute
}

// backendTarget is one backend of a route: its URL, a ready-to-use reverse
// proxy, and its own circuit breaker.
type backendTarget struct {
	url     *url.URL
	proxy   *httputil.ReverseProxy
	breaker *breaker.Breaker // nil means circuit breaking is disabled for this backend
}

// compiledRoute is a route with its backends pre-parsed into ready-to-use
// targets, plus round-robin and rate-limit state.
type compiledRoute struct {
	prefix   string
	backends []*backendTarget
	next     atomic.Uint32

	// limiter is nil when the route has no rate_limit configured, meaning
	// requests to it are unlimited.
	limiter *ratelimit.Limiter
}

// RouteInfo is a read-only summary of a compiled route, used for startup
// logging.
type RouteInfo struct {
	PathPrefix string
	Backends   []string
}

// NewRouter builds a Router from a validated config. It pre-parses every
// backend URL and constructs one reverse proxy per backend up front, so
// request handling does no allocation beyond round-robin selection.
func NewRouter(cfg *config.Config, logger *slog.Logger) (*Router, error) {
	if logger == nil {
		logger = slog.Default()
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

	// Longest prefix first, so matching can stop at the first hit.
	sort.Slice(routes, func(i, j int) bool {
		return len(routes[i].prefix) > len(routes[j].prefix)
	})

	return &Router{routes: routes}, nil
}

// Routes returns a summary of the compiled routes for startup logging.
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

// ServeHTTP matches the request to a route by longest path_prefix,
// enforces that route's rate limit (if configured), and forwards allowed
// requests to the next available backend in the route's round-robin
// rotation, skipping any backend whose circuit breaker is currently Open.
//
// If no route matches, it responds 404 with a JSON error body. If the
// client has exceeded the route's rate limit, it responds 429 with a
// Retry-After header and a JSON error body, without contacting any
// backend. If every backend's breaker is Open, it responds 503 with a
// JSON error body, again without contacting any backend.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := rt.match(r.URL.Path)
	if route == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no route matches path %q", r.URL.Path))
		return
	}

	if !route.allowRequest(w, r) {
		return
	}

	if !route.forward(w, r) {
		writeJSONError(w, http.StatusServiceUnavailable, "circuit open: all backends for this route are currently unavailable")
	}
}

func (rt *Router) match(path string) *compiledRoute {
	for _, r := range rt.routes {
		if strings.HasPrefix(path, r.prefix) {
			return r
		}
	}
	return nil
}

// allowRequest enforces the route's rate limit, if any. When the request
// is not allowed, it writes the 429 response itself (including a
// Retry-After header estimating when a token will next be available) and
// returns false; the caller must not forward the request in that case.
func (cr *compiledRoute) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	if cr.limiter == nil {
		return true
	}

	key := ratelimit.ClientIP(r)
	allowed, retryAfter := cr.limiter.Allow(key)
	if allowed {
		return true
	}

	retryAfterSeconds := int(math.Ceil(retryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
	return false
}

// forward picks the next backend in round-robin order, skipping any whose
// circuit breaker is Open, and proxies the request to it. It reports the
// outcome (backend unreachable, or a 5xx response, counts as failure) back
// to that backend's breaker before returning.
//
// It returns false, having written nothing, if every backend is currently
// unavailable (Open); the caller is responsible for responding in that
// case.
func (cr *compiledRoute) forward(w http.ResponseWriter, r *http.Request) bool {
	n := len(cr.backends)
	start := int(cr.next.Add(1) % uint32(n))

	for i := 0; i < n; i++ {
		bt := cr.backends[(start+i)%n]

		if bt.breaker != nil && !bt.breaker.Allow() {
			continue
		}

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		bt.proxy.ServeHTTP(rec, r)

		if bt.breaker != nil {
			bt.breaker.Report(rec.status < http.StatusInternalServerError)
		}
		return true
	}

	return false
}

// statusRecorder wraps an http.ResponseWriter to capture the status code
// ultimately written, so the caller can classify the response as a
// success or failure for the circuit breaker after ServeHTTP returns.
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

// Flush lets httputil.ReverseProxy stream responses (e.g. chunked bodies)
// through the recorder as it would through the underlying writer directly.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// newReverseProxy builds a ReverseProxy for a single backend target, with a
// custom Director that rewrites the request to the backend's scheme/host
// while preserving the original path and query, and a custom ErrorHandler
// that returns a JSON 502 instead of the default plaintext proxy error.
func newReverseProxy(target *url.URL, logger *slog.Logger) *httputil.ReverseProxy {
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
	}

	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Error("502 backend unreachable",
			"backend", target.String(),
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
