#!/usr/bin/env bash
# Reproduces the headline numbers: the same 1,000-session character-chat
# replay through the baseline (LLM-Gateway behaviour) and the tuned proxy,
# with the primary provider killed mid-run. Needs Go and a local Redis.
#
#   bench/run.sh                 # full run, ~4 minutes
#   SESSIONS=200 bench/run.sh    # quick run
set -euo pipefail
cd "$(dirname "$0")/.."

SESSIONS=${SESSIONS:-1000}
KILL_AFTER=${KILL_AFTER:-40s}
REDIS_URL=${REDIS_URL:-redis://localhost:6379/0}
# Budgets are still checked on every turn, but set high enough that no
# simulated user reaches them: a soft limit would shorten replies and skew cost.
# Background faults on the primary for the whole run, on top of the kill.
FAULTS=${FAULTS:--p429 0.01 -p5xx 0.005 -pstall-first 0.003 -pstall-mid 0.005}
OUT=bench/results
mkdir -p bin "$OUT" "$OUT/logs"

go build -o bin/ ./cmd/proxy ./cmd/fakellm ./cmd/loadgen
if command -v redis-cli >/dev/null; then redis-cli -u "$REDIS_URL" ping >/dev/null || { echo "Redis is not reachable at $REDIS_URL"; exit 1; }; fi

pids=()
cleanup() { for p in "${pids[@]:-}"; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

wait_for() { for _ in $(seq 1 50); do curl -sf "$1" >/dev/null && return 0; sleep 0.2; done; echo "timed out waiting for $1"; exit 1; }

run() {
  local mode=$1
  echo "== $mode: $SESSIONS sessions, primary killed after $KILL_AFTER"
  # shellcheck disable=SC2086
  bin/fakellm -addr :9001 -name openai $FAULTS >"$OUT/logs/$mode-openai.log" 2>&1 & pids+=($!)
  bin/fakellm -addr :9002 -name anthropic >"$OUT/logs/$mode-anthropic.log" 2>&1 & pids+=($!)
  PROXY_MODE=$mode GATEWAY_PORT=8080 REDIS_URL=$REDIS_URL REDIS_PREFIX="bench-$(date +%s)-$mode:" \
  OPENAI_API_KEY=sk-sim OPENAI_BASE_URL=http://localhost:9001/v1 \
  ANTHROPIC_API_KEY=sk-ant-sim ANTHROPIC_BASE_URL=http://localhost:9002/v1 \
  RATE_LIMIT_TPS=5 RATE_LIMIT_BURST=20 BUDGET_SOFT_TOKENS=50000000 BUDGET_HARD_TOKENS=60000000 \
    bin/proxy -config /dev/null >"$OUT/logs/$mode-proxy.log" 2>&1 & pids+=($!)
  wait_for http://localhost:9001/admin/faults
  wait_for http://localhost:9002/admin/faults
  wait_for http://localhost:8080/metrics
  bin/loadgen -label "$mode" -sessions "$SESSIONS" -kill-url http://localhost:9001/admin/kill \
    -kill-after "$KILL_AFTER" -out "$OUT/$mode.json" >/dev/null
  cleanup; pids=()
  sleep 1
}

run baseline
run tuned
bin/loadgen -compare "$OUT/baseline.json,$OUT/tuned.json" | tee bench/RESULTS.md
