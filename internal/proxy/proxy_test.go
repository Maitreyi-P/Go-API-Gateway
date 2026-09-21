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

// TestRouter_CircuitBreaker_TripsAndReturns503 confirms the breaker is
// wired in front of the actual backend call: once a backend's failure
// rate crosses its configured threshold, the gateway must stop calling it
// (proved via a call counter on the fake backend) and instead respond 503
// immediately.
func TestRouter_CircuitBreaker_TripsAndReturns503(t *testing.T) {
	// The backend succeeds for its first two calls, then fails from the
	// third call onward, so the failure rate crosses the 50% threshold
	// exactly on the fourth call (2 failures / 4 total).
	var backendCalls int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&backendCalls, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(backend.Close)

	cfg := &config.Config{
		Routes: []config.Route{
			{
				PathPrefix: "/api",
				Backends:   []string{backend.URL},
				CircuitBreaker: &config.CircuitBreaker{
					FailureThreshold: 0.5,
					WindowSeconds:    60,
					CooldownSeconds:  60,
				},
			},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	wantStatuses := []int{
		http.StatusOK,
		http.StatusOK,
		http.StatusInternalServerError,
		http.StatusInternalServerError, // failure rate hits 2/4 = 50% here; breaker trips right after this response is sent
	}
	for i, want := range wantStatuses {
		resp, err := http.Get(gateway.URL + "/api/ping")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("request %d: status = %d, want %d", i, resp.StatusCode, want)
		}
	}

	if got := atomic.LoadInt32(&backendCalls); got != 4 {
		t.Fatalf("backend called %d times before trip, want 4", got)
	}

	// The breaker should now be Open. The next request must be rejected
	// immediately with 503 and a JSON error body.
	resp, err := http.Get(gateway.URL + "/api/ping")
	if err != nil {
		t.Fatalf("request after trip: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	body := decodeJSON(t, resp)
	if _, ok := body["error"]; !ok {
		t.Errorf("expected JSON error body, got %v", body)
	}

	// Further attempts must all be rejected the same way, without ever
	// reaching the backend again.
	for i := 0; i < 3; i++ {
		resp, err := http.Get(gateway.URL + "/api/ping")
		if err != nil {
			t.Fatalf("request after trip (extra %d): %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("request after trip (extra %d): status = %d, want %d", i, resp.StatusCode, http.StatusServiceUnavailable)
		}
	}

	if got := atomic.LoadInt32(&backendCalls); got != 4 {
		t.Errorf("backend was called %d times after the breaker tripped, want still 4 (no further calls)", got)
	}
}

// TestRouter_CircuitBreaker_SkipsOpenBackendInRoundRobin confirms that when
// a route has multiple backends, round-robin selection skips a backend
// whose breaker has tripped and keeps routing traffic to the healthy one,
// rather than ever returning 503 while a healthy backend remains.
func TestRouter_CircuitBreaker_SkipsOpenBackendInRoundRobin(t *testing.T) {
	var failingCalls, healthyCalls int32

	failingBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&failingCalls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failingBackend.Close)

	healthyBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&healthyCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"backend": "healthy"})
	}))
	t.Cleanup(healthyBackend.Close)

	cfg := &config.Config{
		Routes: []config.Route{
			{
				PathPrefix: "/api",
				Backends:   []string{failingBackend.URL, healthyBackend.URL},
				CircuitBreaker: &config.CircuitBreaker{
					FailureThreshold: 0.5,
					WindowSeconds:    60,
					CooldownSeconds:  60,
				},
			},
		},
	}

	gateway := httptest.NewServer(newRouter(t, cfg))
	t.Cleanup(gateway.Close)

	// Round-robin alternates failing/healthy at first. The failing
	// backend's breaker trips on its very first call (1 failure / 1 total
	// = 100%), so every request after that should land on the healthy
	// backend instead of ever returning 503.
	const requests = 10
	for i := 0; i < requests; i++ {
		resp, err := http.Get(gateway.URL + "/api/ping")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			t.Fatalf("request %d: got 503 even though the healthy backend was available", i)
		}
	}

	if got := atomic.LoadInt32(&failingCalls); got != 1 {
		t.Errorf("failing backend was called %d times, want exactly 1 (it should stop being called once its breaker trips)", got)
	}
	if got := atomic.LoadInt32(&healthyCalls); got != requests-1 {
		t.Errorf("healthy backend was called %d times, want %d", got, requests-1)
	}
}
