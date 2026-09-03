#!/usr/bin/env sh
# End-to-end smoke test against a running Nephtys instance.
#
# Registers a webhook stream, posts one event to it, and reads the stream back
# to confirm the event was ingested — the whole path from HTTP in to a published
# JetStream event, using nothing but curl. A webhook source is used precisely
# because it needs no external network: the test supplies its own event, so a
# failure is Nephtys and never someone else's endpoint being down.
#
# Usage: make smoke   (or: NEPHTYS_ADMIN_TOKEN=... sh scripts/smoke.sh)
set -eu

API="${NEPHTYS_API:-http://127.0.0.1:${NEPHTYS_PORT:-3002}}"
TOKEN="${NEPHTYS_ADMIN_TOKEN:-}"
STREAM_ID="${NEPHTYS_SMOKE_ID:-smoke_check}"
HOOK_PORT="${NEPHTYS_SMOKE_PORT:-3099}"

fail() { echo "smoke: $*" >&2; exit 1; }

if [ -z "$TOKEN" ]; then
	fail "NEPHTYS_ADMIN_TOKEN is not set. Stream management answers 403 without it; set it in .env and restart the instance."
fi

# Ask the instance whether it can accept streams at all. A readiness failure
# here is the answer, not a symptom to chase through the steps below.
#
# The retry without -f is not redundant: -f makes curl exit non-zero on an HTTP
# error *and* discard the body, so the 503 case — instance up, broker gone,
# exactly the state worth reporting — would otherwise print nothing at all.
if ! ready=$(curl -fsS "$API/readyz" 2>/dev/null); then
	detail=$(curl -sS "$API/readyz" 2>&1 || true)
	fail "$API/readyz did not answer 200. Is the instance running (make run) and NATS up (make nats-up)? It said: ${detail:-nothing}"
fi
echo "smoke: instance ready — $ready"

cleanup() {
	curl -fsS -X DELETE -H "Authorization: Bearer $TOKEN" "$API/v1/streams/$STREAM_ID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# A stream left over from an interrupted run would make the register step fail
# with a duplicate-id conflict, so clear it first.
cleanup

echo "smoke: registering webhook stream '$STREAM_ID' on port $HOOK_PORT"
register=$(curl -fsS -X POST "$API/v1/streams" \
	-H "Authorization: Bearer $TOKEN" \
	-H "Content-Type: application/json" \
	-d "{\"id\":\"$STREAM_ID\",\"kind\":\"webhook\",\"topic\":\"nephtys.stream.smoke\",\"webhook\":{\"port\":\"$HOOK_PORT\",\"path\":\"/smoke\"}}") ||
	fail "register failed. A 409 means port $HOOK_PORT is taken — set NEPHTYS_SMOKE_PORT to a free one."
echo "smoke: $register"

echo "smoke: posting one event to the webhook"
curl -fsS -X POST "http://127.0.0.1:$HOOK_PORT/smoke" \
	-H "Content-Type: application/json" \
	-d '{"type":"smoke","value":1}' >/dev/null ||
	fail "the webhook did not accept the event on port $HOOK_PORT. If another process holds that port, set NEPHTYS_SMOKE_PORT to a free one — on Windows a second bind to a port already held can succeed, so registration does not always catch the clash."

# Ingest is asynchronous: the webhook answers as soon as the event is accepted,
# and last_message_at is written as it leaves the pipeline. Poll rather than
# sleep a fixed amount, so a slow machine does not produce a false failure.
#
# The read is guarded rather than left to `set -e`: an unguarded failure inside
# the loop would exit the script with no message at all, which is the one thing
# a smoke test must never do. Whole-second sleeps because POSIX sleep takes an
# integer; the first read happens before any sleep, so the usual case is still
# immediate.
i=0
while [ "$i" -lt 15 ]; do
	if stream=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$API/v1/streams/$STREAM_ID" 2>/dev/null); then
		case "$stream" in
		*'"last_message_at"'*)
			echo "smoke: event ingested and published"
			echo "smoke: $stream"
			echo "smoke: OK"
			exit 0
			;;
		esac
	else
		stream=$(curl -sS -H "Authorization: Bearer $TOKEN" "$API/v1/streams/$STREAM_ID" 2>&1 || true)
	fi
	i=$((i + 1))
	sleep 1
done

fail "the event never reached the stream after ${i}s. Last read: ${stream:-nothing}"
