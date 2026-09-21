package proxy_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Maitreyi-P/Go-API-Gateway/internal/config"
	"github.com/Maitreyi-P/Go-API-Gateway/internal/proxy"
)

// testLogger discards output so test runs stay quiet.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newBackend starts a fake backend that reports its own name and the path
// it received, so tests can tell which backend actually served a request.
func newBackend(t *testing.T, name string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"backend": name,
			"path":    r.URL.Path,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newRouter(t *testing.T, cfg *config.Config) *proxy.Router {
	t.Helper()
	router, err := proxy.NewRouter(cfg, testLogger())
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	return router
}

func decodeJSON(t *testing.T, resp *http.Response) map[string]string {
	t.Helper()
	defer resp.Body.Close()
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body: %v", err)
	}
	return body
}

// TestRouter_Routing covers matching-by-prefix and the 404 case for an
// unmatched path.
func TestRouter_Routing(t *testing.T) {
	users := newBackend(t, "users")
	orders := newBackend(t, "orders")

	cfg := &config.Config{
		Routes: []config.Route{
			{PathPrefix: "/api/users", Backends: []string{users.URL}},
			{PathPrefix: "/api/orders", Backends: []string{orders.URL}},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantBackend string // empty means "don't check"
	}{
		{
			name:        "routes to users backend by longest prefix",
			path:        "/api/users/42",
			wantStatus:  http.StatusOK,
			wantBackend: "users",
		},
		{
			name:        "routes to orders backend by longest prefix",
			path:        "/api/orders/7",
			wantStatus:  http.StatusOK,
			wantBackend: "orders",
		},
		{
			name:       "unmatched path returns 404",
			path:       "/api/unknown",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(gateway.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			body := decodeJSON(t, resp)

			if tc.wantBackend != "" && body["backend"] != tc.wantBackend {
				t.Errorf("backend = %q, want %q", body["backend"], tc.wantBackend)
			}
			if tc.wantStatus == http.StatusNotFound {
				if _, ok := body["error"]; !ok {
					t.Errorf("expected JSON error body, got %v", body)
				}
			}
		})
	}
}

// TestRouter_LongestPrefixWins ensures a more specific route wins over a
// shorter, overlapping one regardless of declaration order.
func TestRouter_LongestPrefixWins(t *testing.T) {
	general := newBackend(t, "general")
	specific := newBackend(t, "specific")

	cfg := &config.Config{
		Routes: []config.Route{
			{PathPrefix: "/api", Backends: []string{general.URL}},
			{PathPrefix: "/api/users", Backends: []string{specific.URL}},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	resp, err := http.Get(gateway.URL + "/api/users/1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := decodeJSON(t, resp)
	if body["backend"] != "specific" {
		t.Errorf("backend = %q, want %q (longest prefix should win)", body["backend"], "specific")
	}
}

// TestRouter_RoundRobin sends multiple requests to a route with two
// backends and verifies traffic is split evenly between them.
func TestRouter_RoundRobin(t *testing.T) {
	a := newBackend(t, "a")
	b := newBackend(t, "b")

	cfg := &config.Config{
		Routes: []config.Route{
			{PathPrefix: "/api", Backends: []string{a.URL, b.URL}},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	const requests = 10
	counts := map[string]int{}
	for i := 0; i < requests; i++ {
		resp, err := http.Get(gateway.URL + "/api/ping")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := decodeJSON(t, resp)
		counts[body["backend"]]++
	}

	if counts["a"] != requests/2 || counts["b"] != requests/2 {
		t.Errorf("expected even 5/5 split across backends, got %v", counts)
	}
}

// TestRouter_BadGateway verifies that a matched but unreachable backend
// produces a 502 with a JSON error body rather than the default plaintext
// proxy error.
func TestRouter_BadGateway(t *testing.T) {
	// Start a server purely to obtain a URL, then close it immediately so
	// the port refuses connections.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	cfg := &config.Config{
		Routes: []config.Route{
			{PathPrefix: "/api", Backends: []string{deadURL}},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	resp, err := http.Get(gateway.URL + "/api/ping")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}

	body := decodeJSON(t, resp)
	if _, ok := body["error"]; !ok {
		t.Errorf("expected JSON error body, got %v", body)
	}
}

// TestRouter_RateLimit_429 confirms the rate-limit middleware is wired in
// front of the reverse proxy: once a client exceeds its route's configured
// burst, the gateway must reject the request with 429 and a Retry-After
// header, and must never forward that rejected request to the backend.
func TestRouter_RateLimit_429(t *testing.T) {
	var backendCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&backendCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"backend": "limited"})
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Config{
		Routes: []config.Route{
			{
				PathPrefix: "/api",
				Backends:   []string{backend.URL},
				RateLimit:  &config.RateLimit{RequestsPerSecond: 1, Burst: 2},
			},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	client := &http.Client{}
	do := func() *http.Response {
		req, err := http.NewRequest(http.MethodGet, gateway.URL+"/api/ping", nil)
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		// Pin the rate-limit key explicitly rather than relying on the
		// loopback source port httptest happens to pick.
		req.Header.Set("X-Forwarded-For", "192.0.2.1")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		return resp
	}

	// The configured burst of 2 should both succeed.
	for i := 0; i < 2; i++ {
		resp := do()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i, resp.StatusCode, http.StatusOK)
		}
		resp.Body.Close()
	}

	// A third request immediately after must be rejected without touching
	// the backend.
	resp := do()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("expected a Retry-After header on the 429 response")
	}

	body := decodeJSON(t, resp)
	if _, ok := body["error"]; !ok {
		t.Errorf("expected JSON error body, got %v", body)
	}

	if got := atomic.LoadInt32(&backendCalls); got != 2 {
		t.Errorf("backend was called %d times, want 2 (the rejected request must not reach the backend)", got)
	}
}
