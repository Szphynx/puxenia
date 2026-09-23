#!/usr/bin/env bash
# stop.sh — cleanly quits puMMa so it stops occupying Shift+Device and
# the audio device, e.g. before switching to a different hack. Does two
# things, not just one:
#
#   1. pkill -x push-mm — kills BOTH the supervisor and its re-exec'd
#      child (see main.go's runSupervisor: they're literally the same
#      binary/name, so one pkill -x call SIGTERMs both at once; -x
#      exact-name match, not -f, for the same reason deploy.sh uses it —
#      see that script's own comment on pkill -f self-matching and
#      killing the wrapping shell before "|| true" ever runs). Both
#      processes dying together (not sequentially) means the supervisor
#      sees its own signal directly and returns without respawning —
#      the auto-respawn logic only fires when the CHILD alone dies
#      unexpectedly, not when both are told to stop.
#   2. Directly calls push-manager's own HTTP API to force display mode
#      back to passthrough and the MIDI filter off — a safety net, not
#      redundant: push-mm's own graceful shutdown (SIGTERM handler)
#      already does this, but push-manager is a separate, always-running
#      service that stays in whatever state it was last told, so if
#      push-mm ever dies without running that shutdown path (a hard
#      kill, a crash), Push would otherwise stay stuck in on-screen
#      takeover / MIDI-intercept mode with nothing left alive to release
#      it. This step doesn't depend on push-mm's own shutdown working.
#
# Usage: ./stop.sh <push-ip> [ssh-key-path] [push-manager-url]
#   PUSH_IP / PUSH_KEY / PUSH_MANAGER_URL env vars work too.
set -euo pipefail

PUSH_IP="${1:-${PUSH_IP:-192.168.3.89}}"
PUSH_KEY="${2:-${PUSH_KEY:-$HOME/xenia-build/pushkey}}"
PM_URL="${3:-${PUSH_MANAGER_URL:-http://localhost:7701}}"

SSH_OPTS=(-o StrictHostKeyChecking=accept-new)
if [ -n "$PUSH_KEY" ]; then
  SSH_OPTS+=(-i "$PUSH_KEY")
fi

echo "== Stopping push-mm on $PUSH_IP =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "pkill -x push-mm || true"

echo "== Resetting push-manager display/MIDI-filter state (safety net) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  "curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d '{\"mode\":0}' $PM_URL/api/display/mode || true; \
   curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d '{\"enabled\":false}' $PM_URL/api/midi/filter || true"

echo "Stopped. Shift+Device and the audio device are free for another hack."
