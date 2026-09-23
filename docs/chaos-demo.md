# Chaos demo walkthrough

This is a demo of the gateway's
resilience features: rate limiting, per-backend circuit breaking, and the
metrics that make both observable. It explains what
`scripts/chaos_demo.sh` does, why each step exists, and exactly what to
look at — in the gateway's logs and in `/metrics` — at each stage.

## What the demo proves

A single backend (`backend-3`, behind the `/api/orders` route) starts
healthy, is made to fail 100% of its requests mid-traffic, and recovers.
Across that, you can show:

- Requests keep succeeding overall (via `/api/users`'s two backends and,
  once it trips, via the gateway's fast rejection of `/api/orders`) rather
  than the whole gateway hanging or crashing.
- The circuit breaker trips from a real observed failure, not a timer.
- Once tripped, the gateway stops calling the dead backend entirely — no
  more slow timeouts, just immediate `503`s.
- The breaker self-heals: after the cooldown, one trial request decides
  whether to close again, and it does once the backend is healthy.
- Every one of these transitions is visible in `gateway_circuit_breaker_state`
  in `/metrics`, in real time, which is the actual point of Phase 4 and 5
  combined — you're not being asked to trust the code, you can watch the
  number change.

## Before you start

The demo script assumes the gateway and mock backends are already
running. If they aren't, it will detect that and print the exact commands
to start them, then exit. To start them yourself ahead of time:

```bash
go run ./cmd/mockbackend -port 9001 -name backend-1
go run ./cmd/mockbackend -port 9002 -name backend-2
go run ./cmd/mockbackend -port 9003 -name backend-3
go run ./cmd/gateway -config config.yaml -port 8080
```

`config.yaml`'s `/api/orders` route (the one this demo targets) has a
single backend (`backend-3` on port 9003) with:

- `rate_limit`: 5 req/s, burst 10
- `circuit_breaker`: 50% failure-rate threshold, 10s window, 15s cooldown

Those two numbers — 10s window and 15s cooldown — are why the demo's
default wait times are what they are (see below).

## What to have open while it runs

Two things, ideally on screen at once alongside the terminal running the
script:

1. **A live metrics watch**, in its own terminal:
   ```bash
   watch -n1 'curl -s http://localhost:8080/metrics | grep -E "circuit_breaker_state|orders"'
   ```
   This is the single most important thing to have visible — you'll
   literally watch `gateway_circuit_breaker_state{backend="http://localhost:9003"}`
   flip `0 → 1 → 2 → 0` (closed → open → half-open → closed) over the
   course of the run.

2. **`load_test.sh`'s own interleaved status lines**, printed in the same
   terminal the demo script runs in. Its failure breakdown by HTTP status
   code is the clearest blow-by-blow account of what's actually happening
   to each request (see Step 4 below).

Note on the gateway's own log: this demo drives failures via the mock
backend's `/chaos` endpoint, which keeps the backend process up and simply
makes it answer with `500` instead of `200` — it never actually goes
unreachable. So you will *not* see the gateway's `"502 backend
unreachable"` error line during this demo; that only fires when a backend
can't be connected to at all (e.g. its process is killed, as in the
Phase 3 manual walkthough). The gateway forwards the backend's real `500`
responses transparently and stays otherwise quiet in its own log for
them — the breaker's state changes are visible in `/metrics`, not in the
log.

The script's own output (and `load_test.sh`'s interleaved status lines)
form a third, self-contained account of the same story, so even a
screen-recording of just the terminal running `chaos_demo.sh` tells the
full story on its own if you don't want to juggle three windows.

## Running it

```bash
scripts/chaos_demo.sh
```

It takes no required arguments; every knob has a default tuned to
`config.yaml`'s circuit breaker settings, and can be overridden via
environment variables if you want a faster or slower demo:

| Variable | Default | Meaning |
|---|---|---|
| `GATEWAY_URL` | `http://localhost:8080` | The gateway to drive traffic through |
| `BACKEND_CHAOS_URL` | `http://localhost:9003/chaos` | Which backend's chaos control endpoint to hit |
| `BACKEND_LABEL` | `http://localhost:9003` | That backend's URL as it appears in metric labels |
| `LOAD_TARGET` | `$GATEWAY_URL/api/orders/1` | The URL `load_test.sh` hammers |
| `CONCURRENCY` | `5` | Parallel `load_test.sh` workers |
| `CHAOS_FAILURE_RATE` | `1.0` | Failure rate injected into the backend (1.0 = always fails) |
| `BASELINE_SECONDS` | `5` | Pause before load starts |
| `TRIP_OBSERVE_SECONDS` | `15` | How long to watch the breaker after injecting chaos |
| `COOLDOWN_WAIT_SECONDS` | `18` | How long to wait for recovery (must exceed the 15s cooldown) |
| `RECOVERY_BUFFER_SECONDS` | `10` | Extra slack added to the background load test's own duration |

## Step-by-step: what happens and what to watch

**Step 1 — Baseline.** Confirms `backend-3`'s chaos settings are
currently healthy (`failure_rate: 0`), sends 3 requests directly through
the gateway, and pauses. *Watch:* all three requests return `200`. This
is the "before" picture.

**Step 2 — Start continuous load.** Launches `load_test.sh` in the
background against `/api/orders/1` with several concurrent workers,
hitting the gateway continuously for the rest of the demo. *Watch:*
`load_test.sh`'s own status lines (`total=... ok=... fail=...`) appear
every 2 seconds, interleaved with the rest of the script's output. Early
on, `fail=0` — everything is succeeding.

Because all the load-test workers run from the same machine, the gateway
sees them as a single client IP. Don't be surprised if you also see a
`429` or two show up in the failure breakdown once concurrency is high
enough to exceed the 5 req/s + burst-10 rate limit on this route — that's
the rate limiter working as designed, not a bug in the chaos test.

**Step 3 — Inject chaos.** `POST`s `{"failure_rate": 1.0, "latency_ms": 0}`
to `backend-3`'s `/chaos` endpoint. From this instant, every request that
reaches `backend-3` gets a `500`. *Watch:* the script echoes the backend's
own confirmation of its new settings.

**Step 4 — Watch it trip.** This is the main event. For the next 15
seconds (by default), the script polls `/metrics` every 3 seconds and
prints the breaker's state. *Watch:*

- In the metrics terminal: `gateway_circuit_breaker_state{backend="http://localhost:9003"}`
  goes from `0` to `1` — almost immediately. With this project's breaker
  design, a fresh failure at a 50% threshold trips on the very first
  observed failure (1 failure / 1 total = 100% ≥ 50%), so you won't be
  waiting long.
- In `load_test.sh`'s status lines: the failure breakdown shifts from
  `500` entries to `503` entries. A `500` means the gateway actually
  called `backend-3` and forwarded its (simulated) failure response
  through; a `503` means the gateway didn't bother calling it at all —
  proof the breaker is doing its job. You should see the `500` count stop
  climbing almost immediately after chaos is injected, replaced by a
  climbing `503` count instead.

**Step 5 — Restore the backend.** `POST`s `{"failure_rate": 0, "latency_ms": 0}`
back to `backend-3`. It's healthy again immediately, but the gateway
doesn't know that yet — it won't try it again until the cooldown elapses.

**Step 6 — Wait for recovery.** Polls the breaker state for 18 seconds
(longer than the 15s cooldown), then sends 3 more direct requests.
*Watch:*

- The gauge stays at `1` (open) for the first ~15 seconds — the cooldown
  is still running, and the gateway is correctly still refusing to call
  `backend-3`.
- Once the cooldown elapses, the *next* request that would have gone to
  `backend-3` becomes a single half-open trial: you may catch the gauge
  at `2` (half-open) for an instant, or you may just see it land directly
  back on `0` (closed) if the trial already resolved between two 3-second
  polls — either is a correct outcome, since the trial resolves in one
  round trip.
- The 3 confirmation requests at the end of this step should all return
  `200`, and the script prints the final state explicitly.

**Step 7 — Stop the load test.** Cleanly terminates the background
`load_test.sh` process (and, via its own signal trap, all of its worker
processes) and prints its final tally.

**Demo complete.** The script's last action is dumping every `gateway_*`
metric line from `/metrics`, so the final state of everything — request
counts by status, the rate-limit rejection count, and the breaker gauge —
is visible in one place at the end, for a closing screenshot.

## Re-running

The script is idempotent — the breaker instance lives inside the running
gateway process, so as long as you leave the gateway up, you can re-run
`scripts/chaos_demo.sh` as many times as you want and it'll walk through
the same sequence again from whatever state the breaker happens to be in
(which will be Closed/healthy, assuming the previous run finished its
recovery step).
