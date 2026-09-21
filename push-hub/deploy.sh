#!/usr/bin/env bash
# deploy.sh — builds (via build.sh) then ships dist/ to a Push 3 over SSH
# and relaunches. Same shape as push-hack-mm/deploy.sh (kill first, since
# scp'ing over a running binary fails with "Text file busy"; ln -s /lib64
# workaround; relaunch) — see docs/environment-and-deploy.md.
#
# Usage: ./deploy.sh <push-ip> [ssh-key-path] [remote-dir]
#   PUSH_IP / PUSH_KEY / PUSH_REMOTE_DIR env vars work too.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"

PUSH_IP="${1:-${PUSH_IP:-}}"
PUSH_KEY="${2:-${PUSH_KEY:-}}"
REMOTE_DIR="${3:-${PUSH_REMOTE_DIR:-/tmp/push-hub}}"

if [ -z "$PUSH_IP" ]; then
  echo "usage: $0 <push-ip> [ssh-key-path] [remote-dir]" >&2
  echo "   (or set PUSH_IP / PUSH_KEY / PUSH_REMOTE_DIR)" >&2
  exit 1
fi

SSH_OPTS=(-o StrictHostKeyChecking=accept-new)
if [ -n "$PUSH_KEY" ]; then
  SSH_OPTS+=(-i "$PUSH_KEY")
fi

"$SCRIPT_DIR/build.sh"

echo "== Stopping any running push-hub on $PUSH_IP =="
# -x (exact process-name match), not -f — see push-hack-mm/deploy.sh's
# comment on why -f can self-match the wrapping ssh/bash invocation.
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "pkill -x push-hub || true"

echo "== Copying to $PUSH_IP:$REMOTE_DIR =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "mkdir -p $REMOTE_DIR"
scp "${SSH_OPTS[@]}" "$DIST_DIR/push-hub" "root@$PUSH_IP:$REMOTE_DIR/"
scp "${SSH_OPTS[@]}" "$DIST_DIR/hack.json" "root@$PUSH_IP:$REMOTE_DIR/"
scp "${SSH_OPTS[@]}" "$DIST_DIR/hacks.json" "root@$PUSH_IP:$REMOTE_DIR/"

echo "== Ensuring /lib64 -> /lib symlink exists (see docs/environment-and-deploy.md) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "[ -e /lib64 ] || ln -s /lib /lib64"

echo "== Relaunching push-hub on $PUSH_IP =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  "cd $REMOTE_DIR && chmod +x push-hub && nohup ./push-hub -config hack.json > push-hub.log 2>&1 &"

echo "Deployed and (re)launched. Logs: ssh ${SSH_OPTS[*]} root@$PUSH_IP tail -f $REMOTE_DIR/push-hub.log"
