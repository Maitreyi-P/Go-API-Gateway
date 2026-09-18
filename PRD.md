# PRD: Rate-Limited API Gateway (Go)

## 1. Overview

A lightweight, self-contained API gateway/reverse proxy that routes traffic to backend services while enforcing per-client rate limits and protecting against cascading failures via circuit breakers. The system exposes Prometheus metrics for full observability and includes a Grafana dashboard for visualization. The goal is a project that demonstrates production-grade systems thinking (resilience, observability, config-driven design) in a scope buildable solo in 1–2 weeks.

**Target roles this signals for:** infrastructure/platform engineering, API/networking (Fastly, Twilio), reliability-focused (PagerDuty). It hits Go proficiency, distributed systems patterns, and ops tooling — all in one cohesive artifact.

## 2. Goals & Non-Goals

**Goals**
- Correctly proxy HTTP requests to one or more configured backends
- Enforce per-client rate limiting (token bucket, hand-rolled)
- Detect failing backends and trip circuit breakers to prevent hammering them
- Expose rich Prometheus metrics
- Be configurable via file, not hardcoded
- Run reproducibly via Docker Compose (gateway + backends + Prometheus + Grafana)
- Have tests that prove the resilience behavior actually works (not just "it compiles")

**Non-Goals** (cut for v1, list as "future work")
- TLS/mTLS termination
- Auth/JWT validation
- Distributed rate limiting across multiple gateway instances (Redis-backed) — good stretch goal
- Load balancing algorithms beyond round-robin
- Full service mesh feature parity

## 3. Architecture

```
                     ┌─────────────┐
   Clients  ───────▶ │  Gateway     │───▶ Backend A
                     │  (Go)        │───▶ Backend B
                     │              │───▶ Backend C
                     └──────┬───────┘
                            │ /metrics
                            ▼
                     ┌─────────────┐      ┌───────────┐
                     │ Prometheus  │ ───▶ │  Grafana  │
                     └─────────────┘      └───────────┘
```

Request flow through the gateway (middleware chain):
`Request → Logging → Rate Limiter → Circuit Breaker (per backend) → Reverse Proxy → Backend`

## 4. Components

| Component | Difficulty | Description |
|---|---|---|
| Reverse proxy core | Easy | Route requests to backends by path prefix or host, using `httputil.ReverseProxy` with custom `Director`/`ErrorHandler` |
| Rate limiter | Easy–Medium | Hand-rolled token bucket, keyed per client IP (or API key), with per-route override support |
| Circuit breaker | Medium | Per-backend state machine (Closed → Open → Half-Open), hand-rolled, inspired by `sony/gobreaker` |
| Load balancer (stretch) | Easy | Round-robin across multiple instances of a backend |
| Observability | Medium | `/metrics` endpoint via `prometheus/client_golang`: request counts, latency histograms, error rates, breaker state gauge, rate-limit rejections |
| Config system | Easy–Medium | YAML config defining routes, backends, rate limits, breaker thresholds; hot-reload is a stretch goal |
| Graceful shutdown | Easy | Drain in-flight requests on SIGTERM/SIGINT |
| Structured logging | Easy | `log/slog` (stdlib, Go 1.21+) — request ID, latency, status, backend |
| Chaos/load testing harness | Medium | Backend that can simulate latency/failures on demand; script to generate load and trigger breaker trips |
| Grafana dashboard | Easy–Medium | JSON dashboard: request rate, p50/p95/p99 latency, error rate, breaker state timeline |
| Containerization | Easy | Dockerfile for gateway, Compose file wiring gateway + mock backends + Prometheus + Grafana |

## 5. Tech Stack

- **Language:** Go 1.22+
- **Core stdlib:** `net/http`, `net/http/httputil`, `log/slog`, `context`, `sync`
- **Metrics:** `github.com/prometheus/client_golang`
- **Config:** `gopkg.in/yaml.v3`
- **Testing:** stdlib `testing` + `httptest`, maybe `testify` for assertions
- **Containerization:** Docker, Docker Compose
- **Dashboards:** Grafana (provisioned via JSON + Compose)
- **Optional stretch:** Redis (distributed rate limiting), `golang-migrate`/SQLite (if you add persistence — probably skip)

## 6. Key Design Decisions to Make Upfront

1. **Rate limit key**: per-IP by default, but design the interface so it could key on an API-key header instead (`X-API-Key`). Makes the resume line "per-client rate limiting" literally true.
2. **Token bucket, hand-rolled**: use `sync.Mutex` + manual refill calculation, rather than `x/time/rate`. More impressive, and forces you to actually understand the algorithm (you'll want to explain this in interviews).
3. **Circuit breaker granularity**: one breaker per backend, not global. State: `Closed`, `Open`, `HalfOpen`. Trip on failure-rate threshold over a sliding window (not just consecutive failures — more realistic).
4. **Config-driven backends**: YAML like:
```yaml
routes:
  - path_prefix: /api/users
    backends: ["http://localhost:9001"]
    rate_limit:
      requests_per_second: 5
      burst: 10
    circuit_breaker:
      failure_threshold: 0.5
      window_seconds: 10
      cooldown_seconds: 15
```
5. **Metrics naming**: follow Prometheus conventions (`gateway_requests_total`, `gateway_request_duration_seconds`, `gateway_circuit_breaker_state`, `gateway_rate_limit_rejections_total`) so the dashboard/PromQL looks idiomatic.

## 7. Build Plan (Milestones)

**Phase 1 — Core proxy (Day 1)**
- Basic reverse proxy with static routing from config
- Health check endpoint per backend
- Manual test: spin up 2 mock backends, confirm routing works

**Phase 2 — Rate limiting (Day 2)**
- Hand-rolled token bucket per client key
- Return `429` with `Retry-After` header when exceeded
- Unit tests: burst allowed, sustained rate rejected, refill over time

**Phase 3 — Circuit breaker (Day 3–4)**
- State machine implementation + unit tests for all transitions
- Wire into proxy: skip backend call when Open, return `503`
- Half-open trial logic with limited probe requests

**Phase 4 — Observability (Day 5)**
- Prometheus instrumentation across all middleware
- `/metrics` endpoint
- Verify metrics with `curl localhost:PORT/metrics` and Prometheus scrape

**Phase 5 — Chaos harness + load testing (Day 6)**
- Mock backend with configurable failure rate / latency injection (env vars or endpoint toggle)
- Load script (Go, `hey`, or `k6`) that ramps traffic and triggers rate limits + breaker trips
- Confirm graceful degradation: some requests succeed, breaker trips, recovers on half-open success

**Phase 6 — Polish (Day 7–8)**
- Graceful shutdown (drain connections, timeout)
- Structured logging with request IDs
- Dockerfile + docker-compose.yml (gateway, 2–3 mock backends, Prometheus, Grafana provisioned with dashboard + datasource)
- README with architecture diagram, setup instructions, screenshots of Grafana dashboard mid-chaos-test

**Phase 7 — Documentation & demo (Day 8–9)**
- README: problem statement, architecture, how to run, how to reproduce the chaos test
- Optional: short GIF/screen recording showing the dashboard reacting live to a load test
- Write up a short "design decisions" doc — this is what turns a toy project into an interview talking point

## 8. Testing Strategy

- **Unit tests**: token bucket math, breaker state transitions, config parsing
- **Integration tests**: `httptest.Server` mock backends, full request flow through middleware chain
- **Load/chaos test**: scripted scenario — ramp traffic past rate limit, kill a backend mid-test, verify breaker trips and other backends keep serving, verify recovery when backend comes back
- This chaos scenario is your demo — it's the proof behind "graceful degradation under simulated load and backend failures" in your resume bullet, so make it repeatable and screenshot/video-able

## 9. Final Deliverable

- **GitHub repo** with:
  - Clean package structure (`/gateway`, `/ratelimit`, `/breaker`, `/metrics`, `/config`, `/cmd/gateway`, `/cmd/mockbackend`)
  - `docker-compose.yml` — one command (`docker compose up`) brings up the full stack
  - Grafana dashboard JSON committed and auto-provisioned
  - README with architecture diagram, setup steps, and a "chaos demo" walkthrough
  - Test suite runnable via `go test ./...`
- **Live artifact**: a short recorded demo (GIF or video) showing the Grafana dashboard reacting to a load test with a simulated backend failure — this is the single most compelling thing you can link from a resume/portfolio
- **Resume bullet**: matches what you already drafted, now backed by a real, runnable, testable project

## 10. Stretch Goals (if time allows)

- Distributed rate limiting via Redis (shows you understand the single-instance limitation)
- Weighted/least-connections load balancing
- Config hot-reload via `fsnotify`
- Basic JWT auth middleware
- OpenTelemetry tracing in addition to metrics