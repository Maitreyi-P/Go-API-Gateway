// Package proxy implements the gateway's routing and reverse-proxy core:
// matching requests to a configured route by longest path prefix, then
// forwarding to one of that route's backends (round-robin if there is more
// than one).
package proxy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
)

// Router matches incoming requests to a route and proxies them to a
// backend. It implements http.Handler.
type Router struct {
	routes []*compiledRoute
}

// compiledRoute is a route with its backends pre-parsed into ready-to-use
// reverse proxies, plus round-robin state.
type compiledRoute struct {
	prefix   string
	backends []*url.URL
	proxies  []*httputil.ReverseProxy
	next     atomic.Uint32
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
		backends := make([]*url.URL, 0, len(r.Backends))
		for _, b := range r.Backends {
			u, err := url.Parse(b)
			if err != nil {
				return nil, fmt.Errorf("route %s: parsing backend %q: %w", r.PathPrefix, b, err)
			}
			backends = append(backends, u)
		}

		cr := &compiledRoute{
			prefix:   r.PathPrefix,
			backends: backends,
			proxies:  make([]*httputil.ReverseProxy, len(backends)),
		}
		for i, target := range backends {
			cr.proxies[i] = newReverseProxy(target, logger)
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
		for i, b := range r.backends {
			backends[i] = b.String()
		}
		out = append(out, RouteInfo{PathPrefix: r.prefix, Backends: backends})
	}
	return out
}

// ServeHTTP matches the request to a route by longest path_prefix and
// forwards it to the next backend in that route's round-robin rotation. If
// no route matches, it responds 404 with a JSON error body.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := rt.match(r.URL.Path)
	if route == nil {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no route matches path %q", r.URL.Path))
		return
	}
	route.nextProxy().ServeHTTP(w, r)
}

func (rt *Router) match(path string) *compiledRoute {
	for _, r := range rt.routes {
		if strings.HasPrefix(path, r.prefix) {
			return r
		}
	}
	return nil
}

// nextProxy returns the next backend's reverse proxy in round-robin order.
func (cr *compiledRoute) nextProxy() *httputil.ReverseProxy {
	if len(cr.proxies) == 1 {
		return cr.proxies[0]
	}
	idx := cr.next.Add(1) % uint32(len(cr.proxies))
	return cr.proxies[idx]
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
		logger.Error("backend unreachable",
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
