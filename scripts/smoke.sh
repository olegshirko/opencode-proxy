#!/bin/sh
# Smoke-test a freshly built binary before it replaces the running one.
#
# The candidate starts on a spare port with the real config; the running
# proxy keeps serving on its own port throughout. Nothing is swapped unless
# /admin/status answers with ok:true.
set -eu

BIN=${1:-./bin/opencode-proxy.new}
CONFIG=${2:-./config.json}
PORT=${3:-4010}

"$BIN" -config "$CONFIG" -addr "127.0.0.1:$PORT" >/tmp/opencode-proxy-smoke.log 2>&1 &
pid=$!
trap 'kill "$pid" 2>/dev/null || true' EXIT INT TERM

i=0
while [ "$i" -lt 60 ]; do
    if curl -sf "http://127.0.0.1:$PORT/admin/status" -o /tmp/opencode-proxy-smoke.json; then
        break
    fi
    i=$((i + 1))
    sleep 0.1
done

if [ ! -s /tmp/opencode-proxy-smoke.json ]; then
    echo "smoke: candidate never answered on port $PORT" >&2
    cat /tmp/opencode-proxy-smoke.log >&2 || true
    exit 1
fi

if ! grep -q '"ok":true' /tmp/opencode-proxy-smoke.json; then
    echo "smoke: /admin/status did not report ok" >&2
    cat /tmp/opencode-proxy-smoke.json >&2
    exit 1
fi

echo "smoke: ok on port $PORT"
