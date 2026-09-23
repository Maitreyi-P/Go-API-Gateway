#!/usr/bin/env bash
# load_test.sh - generate continuous concurrent HTTP load against a URL
# using only curl and bash (no hey/vegeta/wrk dependency).
#
# Usage:
#   scripts/load_test.sh -u URL -d DURATION_SECONDS -c CONCURRENCY
#
# Prints a running total/success/failure count (with a breakdown of
# failure status codes) every 2 seconds while it runs.
set -uo pipefail

usage() {
	cat <<'EOF'
Usage: load_test.sh -u URL -d DURATION -c CONCURRENCY

  -u URL           Target URL to hit repeatedly (required)
  -d DURATION      How long to run, in seconds (required)
  -c CONCURRENCY   Number of parallel workers (required)
  -h               Show this help

Each worker fires requests back-to-back (no delay between them) for the
full duration. Every 2 seconds, prints total/ok/fail counts and a
breakdown of failure responses by HTTP status code.

Requires only curl and bash.
EOF
}

URL=""
DURATION=""
CONCURRENCY=""

while getopts "u:d:c:h" opt; do
	case "$opt" in
		u) URL="$OPTARG" ;;
		d) DURATION="$OPTARG" ;;
		c) CONCURRENCY="$OPTARG" ;;
		h)
			usage
			exit 0
			;;
		*)
			usage
			exit 1
			;;
	esac
done

if [[ -z "$URL" || -z "$DURATION" || -z "$CONCURRENCY" ]]; then
	echo "error: -u, -d, and -c are all required" >&2
	usage
	exit 1
fi

WORKDIR="$(mktemp -d)"
STOP_FILE="$WORKDIR/stop"

cleanup() {
	touch "$STOP_FILE" 2>/dev/null || true
	wait 2>/dev/null
	rm -rf "$WORKDIR"
}
trap cleanup EXIT
trap 'exit 0' INT TERM

# One worker: fires requests continuously until STOP_FILE appears,
# appending one line per request ("ok" or "fail:<status>") to its own
# file. Each worker writes only to its own file, so there's no need for
# any file-locking to aggregate counts safely.
worker() {
	local id="$1"
	local outfile="$WORKDIR/worker-$id.log"
	: >"$outfile"
	while [[ ! -f "$STOP_FILE" ]]; do
		status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$URL" 2>/dev/null)"
		if [[ "$status" =~ ^2[0-9][0-9]$ ]]; then
			echo "ok" >>"$outfile"
		else
			echo "fail:${status:-000}" >>"$outfile"
		fi
	done
}

echo "[load_test] starting $CONCURRENCY workers against $URL for ${DURATION}s"

for ((i = 0; i < CONCURRENCY; i++)); do
	worker "$i" &
done

print_status() {
	local elapsed="$1"
	local ok fail total breakdown
	ok=$(cat "$WORKDIR"/worker-*.log 2>/dev/null | grep -c '^ok$')
	fail=$(cat "$WORKDIR"/worker-*.log 2>/dev/null | grep -c '^fail:')
	total=$((ok + fail))
	breakdown=$(cat "$WORKDIR"/worker-*.log 2>/dev/null |
		grep '^fail:' | sed 's/^fail://' | sort | uniq -c |
		awk '{printf "%s:%s ", $2, $1}')
	printf '[load_test] %s  t+%ss  total=%d  ok=%d  fail=%d%s\n' \
		"$(date +%H:%M:%S)" "$elapsed" "$total" "$ok" "$fail" \
		"${breakdown:+  (by status: $breakdown)}"
}

START_TS=$(date +%s)
END_TS=$((START_TS + DURATION))

while [[ $(date +%s) -lt $END_TS ]]; do
	sleep 2
	print_status "$(($(date +%s) - START_TS))"
done

touch "$STOP_FILE"
wait

print_status "$(($(date +%s) - START_TS))"
echo "[load_test] done"
