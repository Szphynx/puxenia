#!/usr/bin/env bash
# stop.sh — kills push-sample-fetch on Push. No display/MIDI state to
# reset (unlike push-hub/push-hack-mm's stop.sh) since this tool never
# touches push-manager/push-display at all.
#
# Usage: ./stop.sh <push-ip> [ssh-key-path]
#   PUSH_IP / PUSH_KEY env vars work too.
set -euo pipefail

PUSH_IP="${1:-${PUSH_IP:-}}"
PUSH_KEY="${2:-${PUSH_KEY:-}}"

if [ -z "$PUSH_IP" ]; then
  echo "usage: $0 <push-ip> [ssh-key-path]  (or set PUSH_IP / PUSH_KEY)" >&2
  exit 1
fi

SSH_OPTS=(-o StrictHostKeyChecking=accept-new)
if [ -n "$PUSH_KEY" ]; then
  SSH_OPTS+=(-i "$PUSH_KEY")
fi

ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "pkill -x push-sample-fetch || true"
echo "Stopped."
