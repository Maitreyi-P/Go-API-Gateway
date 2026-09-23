# Go API Gateway

A lightweight, self-contained API gateway in Go: it reverse-proxies HTTP traffic to
one or more backend services while enforcing per-client rate limits and protecting
those backends from cascading failure with per-backend circuit breakers. Both the
rate limiter (token bucket) and the circuit breaker (Closed → Open → Half-Open state
machine) are hand-rolled rather than pulled from a library, specifically so the
project demonstrates actually understanding the algorithms, not just wiring one up.
Everything the gateway does is exported as Prometheus metrics and visualized in a
pre-provisioned Grafana dashboard, so its behavior under load and failure is
something you can *watch*, not just take on faith.

## Architecture

```
                                   Docker network (gateway-net)
                        ┌──────────────────────────────────────────────┐
                        │                                                │
   Clients ────────────▶│   Gateway  :8080                              │
  (curl, load_test.sh)  │   ┌──────────────────────────────────────┐    │
                        │   │ Request                               │    │
                        │   │   → Rate Limiter (token bucket)       │    │
                        │   │   → Circuit Breaker (per backend)     │    │
                        │   │   → Reverse Proxy                     │    │
                        │   └───────────┬──────────────┬────────────┘    │
                        │               │              │                 │
                        │               ▼              ▼                 │
                        │      backend-a :9001   backend-b :9002         │
                        │      (mock backend,     (mock backend,         │
                        │       chaos-capable)      chaos-capable)       │
                        │               │                                │
                        │               │ /metrics                      │
                        │               ▼                                │
                        │        Prometheus :9090  ──scrapes gateway──┐  │
                        │               │            every 5s        │  │
                        │               ▼                             │  │
                        │        Grafana :3000  (auto-provisioned:     │  │
                        │        Prometheus datasource + dashboard)    │  │
                        └──────────────────────────────────────────────┘
```

Request flow through the gateway: **Request → Rate Limiter → Circuit Breaker (per
backend) → Reverse Proxy → Backend**. A route with multiple backends round-robins
between them, skipping any backend whose breaker is currently Open.

## Quickstart

```bash
docker compose up -d --build
```

That's the whole setup — no manual configuration, no clicking through UIs. Once it's
up:

| Service | URL | Notes |
|---|---|---|
| Gateway | http://localhost:8080/api/users/1 | Try `/api/users/*` and `/api/orders/*` |
| Gateway metrics | http://localhost:8080/metrics | Raw Prometheus exposition format |
| Prometheus | http://localhost:9090 | Check **Status → Targets** to see the gateway scrape |
| Grafana | http://localhost:3000 | Login `admin` / `admin` — dashboard is already there |

In Grafana, open **Dashboards → API Gateway**. It's provisioned automatically on
startup (datasource and dashboard JSON both live in `deploy/grafana/`), so there's
nothing to import.

```bash
docker compose down          # stop everything
docker compose logs -f gateway   # structured JSON logs, live
```

### Running locally without Docker

```bash
go run ./cmd/mockbackend -port 9001 -name backend-1
go run ./cmd/mockbackend -port 9002 -name backend-2
go run ./cmd/gateway -config config.yaml -port 8080
```

This is the same setup the test suite and demo scripts assume when you're iterating
on the code directly rather than through Compose (`config.yaml` uses `localhost`
backend URLs for this reason; `config.docker.yaml` is the Compose-specific variant
that uses Compose service names instead).

## Chaos demo

`scripts/chaos_demo.sh` orchestrates a full, narrated demo: baseline traffic →
continuous load → inject a failing backend → watch its circuit breaker trip → restore
it → watch it recover. It works against either the local setup above or the running
Docker Compose stack, since backend ports are published to the host in both cases.

```bash
docker compose up -d --build
scripts/chaos_demo.sh
```

While it runs, keep Grafana open on the **API Gateway** dashboard and watch:

- **Circuit Breaker State per Backend** — the panel to watch most closely. It'll sit
  at `Closed` (green), flip to `Open` (red) within seconds of chaos being injected,
  and flip back to `Closed` once the backend is restored and the cooldown elapses.
- **Request Rate** and **Rate Limit Rejections** — both climb sharply once
  `load_test.sh`'s concurrent workers start hammering the gateway (they all share one
  client IP, so the rate limiter kicks in hard — that's expected, not a bug).
- **Error Rate** — spikes while the backend is failing and the breaker is still
  Closed (real `500`s passing through), stays elevated once it's Open (now `503`s
  instead, but the gateway stops actually calling the dead backend), and recovers
  once the breaker closes again.

`docs/chaos-demo.md` has the full step-by-step walkthrough — what each step does,
why, and exactly what to watch for — written to be read right before recording a
demo.

## Design decisions (talking points)

- **Hand-rolled token bucket, not `x/time/rate`.** Tracks tokens and last-refill
  time per client key behind a mutex, computing elapsed-time refill on each check.
  The point isn't that this beats a library — it's proof of understanding the
  algorithm well enough to explain it, refill math and all, in an interview.
- **Hand-rolled circuit breaker, not `sony/gobreaker`.** A real three-state machine
  (Closed/Open/Half-Open) that trips on a *failure rate* over a rolling time window,
  not just N consecutive failures — closer to how production breakers actually work.
  One known simplification: there's no minimum sample size, so a breaker's very
  first observed failure can trip it outright (1/1 = 100% ≥ any threshold ≤ 100%).
  That's a deliberate trade-off for a small demo project, not an oversight — production
  breakers (Hystrix, resilience4j) usually add a minimum-volume gate to avoid a single
  blip tripping the circuit, which would be the natural next step here.
- **One breaker per backend, not per route.** A route with two backends has two
  independent breakers, so one dead backend doesn't take the healthy one down with
  it — the gateway just stops round-robin-selecting the tripped one.
- **Structured logging throughout, via `log/slog`.** Every request, rate-limit
  rejection, and breaker state transition is logged with the fields you'd actually
  want in a log aggregator (route, backend, status, latency, client identifier),
  defaulting to JSON (`-log-format json|text`) so it's machine-parseable out of the
  box in the container, human-readable on demand while developing locally.
- **Metrics registered on an explicit `prometheus.Registry`, never the global
  default registerer** — every test that touches metrics builds its own isolated
  registry, so metric assertions can't leak state between tests.

## Tests

```bash
go test ./...          # full suite
go test ./... -v -race # verbose, with the race detector
```

Covers: token-bucket math (burst, sustained-rate rejection, refill over time,
independent per-client buckets) and circuit-breaker state transitions in
`internal/ratelimit`/`internal/breaker` using an injectable fake clock (no real
`time.Sleep` in tests), plus full-chain integration tests in `internal/proxy` using
`httptest` fake backends — routing, round-robin, 404/429/502/503 handling, and
metrics assertions via `prometheus/client_golang/prometheus/testutil`.
