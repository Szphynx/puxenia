#!/usr/bin/env bash
# stop.sh — cleanly quits push-hub so it stops occupying Shift+Device and
# the screen. Same two-step safety-net shape as push-hack-mm/stop.sh: kill
# the process, then directly force push-manager's display/MIDI-filter
# state back to passthrough in case push-hub died without releasing them
# itself.
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

echo "== Stopping push-hub on $PUSH_IP =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "pkill -x push-hub || true"

echo "== Resetting push-manager display/MIDI-filter state (safety net) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  "curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d '{\"mode\":0}' $PM_URL/api/display/mode || true; \
   curl -s -o /dev/null -X POST -H 'Content-Type: application/json' -d '{\"enabled\":false}' $PM_URL/api/midi/filter || true"

echo "Stopped. Shift+Device and the screen are free for another hack."
