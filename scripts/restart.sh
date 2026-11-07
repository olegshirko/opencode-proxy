#!/bin/sh
# Restart the launchd service without cutting a streaming response in half.
#
# The server stops accepting connections on SIGTERM and drains in-flight
# requests (up to 30 s), and launchd runs it again because of KeepAlive. So the
# restart only has to wait for an idle moment before signalling; rebooting the
# job with bootout/bootstrap is kept for the case where the plist itself
# changed, because bootout is asynchronous and bootstrapping too early is what
# answers "Input/output error".
set -eu

LABEL=${LABEL:-ai.opencode.proxy}
PORT=${PORT:-4000}
BASE="http://127.0.0.1:$PORT"
UID_N=$(id -u)
ROOT=${ROOT:-$HOME/opencode-proxy}
PLIST=${PLIST:-$HOME/Library/LaunchAgents/$LABEL.plist}
IDLE_TIMEOUT=${IDLE_TIMEOUT:-120}

status() { curl -sf -m 2 "$BASE/admin/status" 2>/dev/null || true; }
healthy() { status | grep -q '"ok":true'; }
loaded() { launchctl print "gui/$UID_N/$LABEL" >/dev/null 2>&1; }
pid() {
    launchctl print "gui/$UID_N/$LABEL" 2>/dev/null |
        awk '$1 == "pid" && $2 == "=" { print $3; exit }'
}

# A restart is only safe while nothing is streaming: the drain cannot outlast
# a request that is itself waiting for this script to return.
wait_idle() {
    i=0
    while [ "$i" -lt "$IDLE_TIMEOUT" ]; do
        if status | grep -q '"in_flight":0'; then
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done
    echo "restart: still busy after ${IDLE_TIMEOUT}s, letting the drain handle it" >&2
}

# wait_healthy OLD_PID — prints the pid of a healthy process that is not
# OLD_PID, so a still-draining old process is never mistaken for the new one.
wait_healthy() {
    old_pid=$1
    i=0
    while [ "$i" -lt 400 ]; do
        current=$(pid || true)
        if [ -n "$current" ] && [ "$current" != "$old_pid" ] && healthy; then
            echo "$current"
            return 0
        fi
        i=$((i + 1))
        sleep 0.1
    done
    return 1
}

reload_plist() {
    launchctl bootout "gui/$UID_N/$LABEL" 2>/dev/null || true
    i=0
    while loaded; do
        i=$((i + 1))
        if [ "$i" -gt 100 ]; then
            echo "restart: job did not unload" >&2
            return 1
        fi
        sleep 0.1
    done
    i=0
    while [ "$i" -lt 10 ]; do
        if launchctl bootstrap "gui/$UID_N" "$PLIST" 2>/dev/null; then
            return 0
        fi
        i=$((i + 1))
        sleep 1
    done
    return 1
}

if loaded; then
    wait_idle
fi

if [ -f "$PLIST.changed" ] || ! loaded; then
    if ! reload_plist; then
        echo "restart: bootstrap failed, see $ROOT/service.err.log" >&2
        exit 1
    fi
    rm -f "$PLIST.changed"
    new=$(wait_healthy "") || {
        echo "restart: no healthy process after bootstrap" >&2
        exit 1
    }
    echo "restart: reloaded $LABEL, pid=$new"
    exit 0
fi

old=$(pid || true)
launchctl kill SIGTERM "gui/$UID_N/$LABEL"
new=$(wait_healthy "$old") || {
    # launchd did not bring it back on its own: fall back to a full reload,
    # which is the path that needs bootout before bootstrap.
    echo "restart: no new process after SIGTERM, reloading the job" >&2
    if ! reload_plist; then
        echo "restart: bootstrap failed, see $ROOT/service.err.log" >&2
        exit 1
    fi
    new=$(wait_healthy "") || {
        echo "restart: $LABEL did not come back healthy" >&2
        tail -n 20 "$ROOT/service.err.log" >&2 2>/dev/null || true
        exit 1
    }
}
echo "restart: pid $old -> $new, healthy"
