#!/usr/bin/env bash
# chaos_demo.sh - orchestrates a full chaos/load demo against a running
# gateway + mock backends: baseline traffic, continuous load, inject a
# failing backend, watch its circuit breaker trip, restore it, and watch
# it recover. See docs/chaos-demo.md for the full walkthrough of what to
# look at during each step.
#
# Assumes the gateway and mock backends are ALREADY RUNNING (see the
# reminder this script prints if they aren't reachable).
set -uo pipefail

# --- Configuration (override via environment variables) -----------------
GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
# The one backend this demo targets directly with chaos: backend-a, which
# is the single backend behind /api/orders in config.docker.yaml (the
# Docker Compose stack this script is designed to run against - see
# docker-compose.yml, which publishes backend-a on host port 9001). Using
# the single backend behind /api/orders (rather than one of the two behind
# /api/users) keeps the demo unambiguous: there's no round-robin partner
# masking whether a given request actually reached it.
#
# BACKEND_CHAOS_URL is reached from the host running this script, so it
# uses localhost + the published port. BACKEND_LABEL must instead match
# the backend URL string the gateway itself uses internally (the Compose
# service name, not localhost), since that's what appears in the
# gateway_circuit_breaker_state metric label.
#
# Running this against a local (non-Docker) `go run` setup instead? Since
# config.yaml uses a 3rd, distinct backend on port 9003 for /api/orders,
# override both: BACKEND_CHAOS_URL=http://localhost:9003/chaos
# BACKEND_LABEL=http://localhost:9003
BACKEND_CHAOS_URL="${BACKEND_CHAOS_URL:-http://localhost:9001/chaos}"
BACKEND_LABEL="${BACKEND_LABEL:-http://backend-a:9001}"
LOAD_TARGET="${LOAD_TARGET:-$GATEWAY_URL/api/orders/1}"
CONCURRENCY="${CONCURRENCY:-5}"
CHAOS_FAILURE_RATE="${CHAOS_FAILURE_RATE:-1.0}"

BASELINE_SECONDS="${BASELINE_SECONDS:-5}"
TRIP_OBSERVE_SECONDS="${TRIP_OBSERVE_SECONDS:-15}"
COOLDOWN_WAIT_SECONDS="${COOLDOWN_WAIT_SECONDS:-18}"
RECOVERY_BUFFER_SECONDS="${RECOVERY_BUFFER_SECONDS:-10}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOAD_TEST="$SCRIPT_DIR/load_test.sh"

# How long load_test.sh should run once started in step 2: covers the
# short pause before injecting chaos, the trip-observation window, the
# cooldown wait, and a little buffer, with a bit of slack. Step 7 stops it
# explicitly regardless, so this only needs to comfortably outlast steps
# 3-6.
LOAD_DURATION=$((3 + TRIP_OBSERVE_SECONDS + COOLDOWN_WAIT_SECONDS + RECOVERY_BUFFER_SECONDS + 5))

log() {
	printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"
}

section() {
	echo
	printf '=== %s ===\n' "$*"
}

breaker_state() {
	curl -s --max-time 2 "$GATEWAY_URL/metrics" |
		grep "gateway_circuit_breaker_state{backend=\"$BACKEND_LABEL\"" |
		awk '{print $NF}'
}

state_name() {
	case "$1" in
		0) echo "closed" ;;
		1) echo "open" ;;
		2) echo "half-open" ;;
		*) echo "unknown" ;;
	esac
}

require_reachable() {
	local url="$1" what="$2"
	if ! curl -s -o /dev/null --max-time 2 "$url"; then
		{
			echo "error: cannot reach $what at $url"
			echo
			echo "Start the Docker Compose stack first, from the repo root:"
			echo "  docker compose up -d --build"
			echo
			echo "(Running against a local, non-Docker \`go run\` setup instead?"
			echo "Override BACKEND_CHAOS_URL and BACKEND_LABEL - see the comments"
			echo "at the top of this script.)"
			echo
			echo "Then re-run this script."
		} >&2
		exit 1
	fi
}

LOAD_PID=""
cleanup() {
	if [[ -n "$LOAD_PID" ]] && kill -0 "$LOAD_PID" 2>/dev/null; then
		kill "$LOAD_PID" 2>/dev/null || true
		wait "$LOAD_PID" 2>/dev/null || true
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM

section "Pre-flight checks"
require_reachable "$GATEWAY_URL/metrics" "the gateway"
require_reachable "$BACKEND_CHAOS_URL" "the chaos-controlled backend ($BACKEND_CHAOS_URL)"
log "gateway ($GATEWAY_URL) and backend ($BACKEND_LABEL) are both reachable"

section "Step 1: baseline (no chaos, no load yet)"
log "current chaos settings on $BACKEND_LABEL:"
curl -s "$BACKEND_CHAOS_URL"
echo
log "sending 3 baseline requests through the gateway to $LOAD_TARGET"
for i in 1 2 3; do
	curl -s -o /dev/null -w "  request $i -> %{http_code}\n" "$LOAD_TARGET"
done
log "pausing ${BASELINE_SECONDS}s before starting load"
sleep "$BASELINE_SECONDS"

section "Step 2: starting continuous load"
log "launching load_test.sh: $CONCURRENCY workers against $LOAD_TARGET for ${LOAD_DURATION}s"
"$LOAD_TEST" -u "$LOAD_TARGET" -d "$LOAD_DURATION" -c "$CONCURRENCY" &
LOAD_PID=$!
log "load test running in the background (pid $LOAD_PID); its own status lines are interleaved below"
sleep 3

section "Step 3: injecting chaos into $BACKEND_LABEL"
FAILURE_PCT=$(awk "BEGIN{printf \"%.0f\", $CHAOS_FAILURE_RATE * 100}")
log "POST $BACKEND_CHAOS_URL  {\"failure_rate\": $CHAOS_FAILURE_RATE, \"latency_ms\": 0}"
curl -s -X POST "$BACKEND_CHAOS_URL" \
	-H 'Content-Type: application/json' \
	-d "{\"failure_rate\": $CHAOS_FAILURE_RATE, \"latency_ms\": 0}"
echo
log "$BACKEND_LABEL is now failing ~${FAILURE_PCT}% of requests"

section "Step 4: watching the breaker trip (${TRIP_OBSERVE_SECONDS}s)"
cat <<EOF
While this runs, in another terminal you can also watch:
  - metrics directly:
      watch -n1 'curl -s $GATEWAY_URL/metrics | grep circuit_breaker_state'
  - load_test.sh's own fail breakdown above: watch its 500 count (real
    failure responses from $BACKEND_LABEL, since chaos makes it respond
    with 500 rather than going unreachable) stop climbing and its 503
    count take over once the breaker is open (a 503 means the gateway
    rejected the request without even calling $BACKEND_LABEL). Note: this
    demo doesn't produce 502s, since the backend process stays up the
    whole time and only its response content changes; 502 only shows up
    if a backend is genuinely unreachable (e.g. its process is killed).
EOF
for ((elapsed = 0; elapsed < TRIP_OBSERVE_SECONDS; elapsed += 3)); do
	sleep 3
	state="$(breaker_state)"
	log "$BACKEND_LABEL breaker state = ${state:-?} ($(state_name "${state:-}"))"
done

section "Step 5: restoring $BACKEND_LABEL to healthy"
log "POST $BACKEND_CHAOS_URL  {\"failure_rate\": 0, \"latency_ms\": 0}"
curl -s -X POST "$BACKEND_CHAOS_URL" \
	-H 'Content-Type: application/json' \
	-d '{"failure_rate": 0, "latency_ms": 0}'
echo
log "$BACKEND_LABEL is healthy again, but the gateway won't use it until the breaker's cooldown elapses and a half-open trial succeeds"

section "Step 6: waiting past the cooldown (${COOLDOWN_WAIT_SECONDS}s) to observe recovery"
for ((elapsed = 0; elapsed < COOLDOWN_WAIT_SECONDS; elapsed += 3)); do
	sleep 3
	state="$(breaker_state)"
	log "$BACKEND_LABEL breaker state = ${state:-?} ($(state_name "${state:-}"))"
done
log "sending 3 more requests directly to confirm recovery"
for i in 1 2 3; do
	curl -s -o /dev/null -w "  request $i -> %{http_code}\n" "$LOAD_TARGET"
done
final_state="$(breaker_state)"
log "final $BACKEND_LABEL breaker state = ${final_state:-?} ($(state_name "${final_state:-}"))"
if [[ "$final_state" == "0" ]]; then
	log "recovered: the half-open trial succeeded and the breaker closed again"
else
	log "not yet closed - if it shows 2 (half-open), the next request to reach it will decide; if still 1 (open), give it a few more seconds"
fi

section "Step 7: stopping the load test"
if kill -0 "$LOAD_PID" 2>/dev/null; then
	kill "$LOAD_PID" 2>/dev/null || true
	wait "$LOAD_PID" 2>/dev/null || true
	log "load test stopped"
else
	wait "$LOAD_PID" 2>/dev/null || true
	log "load test had already finished on its own"
fi
LOAD_PID=""

section "Demo complete"
log "final gateway_* metrics snapshot:"
curl -s "$GATEWAY_URL/metrics" | grep -E '^gateway_'
