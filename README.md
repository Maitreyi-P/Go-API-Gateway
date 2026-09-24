# Go API Gateway

This is a lightweight, self-contained API gateway in Go. 
It reverse-proxies HTTP traffic to one or more backend services while enforcing per-client 
rate limits and protecting those backends from cascading failure with per-backend circuit breakers. 
Both the rate limiter (token bucket) and the circuit breaker (closed → open → half-open state
machine) are written from scratch.
Everything the gateway does is exported as Prometheus metrics and visualized in a
Grafana dashboard

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

Once it's up:

| Service | URL | Notes |
|---|---|---|
| Gateway | http://localhost:8080/api/users/1 | Try `/api/users/*` and `/api/orders/*` |
| Gateway metrics | http://localhost:8080/metrics | Raw Prometheus exposition format |
| Prometheus | http://localhost:9090 | Check **Status → Targets** to see the gateway scrape |
| Grafana | http://localhost:3000 | Login `admin` / `admin` — dashboard is already there |

In Grafana, open **Dashboards → API Gateway**. It's provisioned automatically on
startup.

```bash
docker compose down          # stop everything
docker compose logs -f gateway   # live structured JSON logs
```

### Running locally without Docker

```bash
go run ./cmd/mockbackend -port 9001 -name backend-1
go run ./cmd/mockbackend -port 9002 -name backend-2
go run ./cmd/gateway -config config.yaml -port 8080
```

This is the same setup the test suite and demo scripts assume when running code
directly instead of usinh Compose (`config.yaml` uses `localhost`
backend URLs; `config.docker.yaml` is the Compose-specific and uses
Compose service names instead).

## Chaos demo

`scripts/chaos_demo.sh` runs a full demo of simulated traffic: baseline traffic →
continuous load → inject a failing backend → watch its circuit breaker trip → restore
it → watch it recover.

```bash
docker compose up -d --build
scripts/chaos_demo.sh
```

While it runs, The Grafana **API Gateway** dashboard shows:

- **Circuit Breaker State per Backend** — This shows the state go from 
 `Closed` (green), flip to `Open` (red) within seconds of chaos being injected,
  and flip back to `Closed` once the backend is restored and the cooldown elapses.
- **Request Rate** and **Rate Limit Rejections** — both climb sharply with increased requests on the gateway 
  till the rate-limiter kicks in.
- **Error Rate** — spikes while the backend is failing and the breaker is still
  Closed, stays up once it's Open and recovers once the breaker closes again.

`docs/chaos-demo.md` has step-by-step rundown of the chaos demo. 

## Design decisions

- **Token Bucket (Rate-limiter).** Tracks tokens and last-refill time per client key behind a mutex,
  computing elapsed-time refill on each check.
- **Circuit Breaker.** A three-state machine (Closed/Open/Half-Open) that trips on a 
  *failure rate* over a rolling time window
- **One breaker per backend** A route with two backends has two
  independent breakers,so if one backend dies, the gateway just stops selecting it.
- **Structured logging ** Every action is logged in JSON (`-log-format json|text`).
- **Metrics registered on an explicit `prometheus.Registry`** — every test builds its own isolated
  registry instead of using the global default registerer, so metrics can't leak state between tests.

## Tests

```bash
go test ./...          # full suite
go test ./... -v -race # verbose, with the race detector
```

