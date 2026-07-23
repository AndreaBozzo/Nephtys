#!/usr/bin/env bash
# run-load-level.sh — run one point of the power-vs-throughput curve, end to end.
#
# Sequence (Pi-side operations happen OUTSIDE the measurement window, so no spurious
# load is added to the device under test):
#   1) create an SSE stream on the Pi that consumes from the generator on the host
#   2) let it settle, zero the generator counters
#   3) measure wall power for the requested duration
#   4) collect effective throughput and Nephtys metrics
#   5) delete the stream, leaving the Pi clean for the next point
#
# Usage: ./run-load-level.sh <label> <rate_ev_s> <duration_s>
# Env:   PI (default andrea@pi5.local), TOKEN, PC_IP (host LAN ip), SHELLY_IP, GEN_PORT

set -uo pipefail

# NB: no apostrophes inside ${var:?...} — bash treats them as an opening quote and
# swallows following lines.
LABEL="${1:?label required}"
RATE="${2:?rate in ev/s required}"
DUR="${3:-1200}"

PI="${PI:-andrea@pi5.local}"
TOKEN="${TOKEN:-demo-admin-token}"
PC_IP="${PC_IP:?PC_IP (host LAN ip reachable from the Pi) required}"
GEN_PORT="${GEN_PORT:-8099}"
GEN="http://${PC_IP}:${GEN_PORT}"
STREAM_ID="bench_${LABEL}"
HERE="$(cd "$(dirname "$0")" && pwd)"

say() { echo ">> $*" >&2; }

# --- 1) create the stream --------------------------------------------------
say "creating stream '${STREAM_ID}' -> ${GEN}/sse?rate=${RATE}"
CONFIG=$(cat <<EOF
{
  "id": "${STREAM_ID}",
  "kind": "sse",
  "url": "${GEN}/sse?rate=${RATE}",
  "topic": "nephtys.stream.bench.${LABEL}",
  "pipeline": {}
}
EOF
)

printf '%s' "$CONFIG" | ssh "$PI" "cat > /tmp/${STREAM_ID}.json"

ssh "$PI" "cd ~/Nephtys && ./nephtys --config-check /tmp/${STREAM_ID}.json" >&2 || {
  say "ERROR: invalid config, aborting"; exit 1;
}

ssh "$PI" "curl -s -X POST -H 'Authorization: Bearer ${TOKEN}' -H 'Content-Type: application/json' --data @/tmp/${STREAM_ID}.json localhost:3002/v1/streams -w ' [HTTP %{http_code}]\n'" >&2

# --- 2) settle + zero counters ---------------------------------------------
say "settling for 15s..."
sleep 15
curl -s -m 5 "${GEN}/reset" >/dev/null
say "counters reset, starting measurement"

# --- 3) measure ------------------------------------------------------------
RESULT="$(SHELLY_IP="${SHELLY_IP:-}" "${HERE}/measure-power.sh" "$LABEL" "$DUR" 2)"

# --- 4) effective throughput + Nephtys metrics -----------------------------
say "--- generator-side effective throughput ---"
curl -s -m 5 "${GEN}/stats" >&2

say "--- Nephtys metrics + stream state ---"
ssh "$PI" "curl -s -H 'Authorization: Bearer ${TOKEN}' localhost:3002/v1/streams; echo; curl -s localhost:3002/metrics | grep -E '^(bytes_ingested_total|bytes_published_total|event_processing_duration_seconds_count|event_processing_duration_seconds_sum)' | head -12; echo; echo \"CPU temp: \$(vcgencmd measure_temp)  throttled: \$(vcgencmd get_throttled)\"" >&2

# --- 5) cleanup ------------------------------------------------------------
say "deleting stream '${STREAM_ID}'"
ssh "$PI" "curl -s -X DELETE -H 'Authorization: Bearer ${TOKEN}' localhost:3002/v1/streams/${STREAM_ID} -w ' [HTTP %{http_code}]\n'" >&2

echo "$RESULT"
