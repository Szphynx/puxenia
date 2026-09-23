#!/usr/bin/env bash
# deploy.sh — builds (via build.sh) then ships dist/ to a Push 3 over SSH
# and relaunches. Mirrors docs/environment-and-deploy.md's "Full deploy
# loop" (written for Xenia) — same steps (kill first, since scp'ing over a
# running binary fails with "Text file busy"; ln -s /lib64 workaround;
# relaunch), just scripted for this hack.
#
# Usage: ./deploy.sh <push-ip> [ssh-key-path] [remote-dir]
#   PUSH_IP / PUSH_KEY / PUSH_REMOTE_DIR env vars work too.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"

PUSH_IP="${1:-${PUSH_IP:-}}"
PUSH_KEY="${2:-${PUSH_KEY:-}}"
REMOTE_DIR="${3:-${PUSH_REMOTE_DIR:-/tmp/mm-hack}}"

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

if ! compgen -G "$DIST_DIR/module/roms/*.bin" > /dev/null; then
  echo "warning: no ROM found in $DIST_DIR/module/roms/ — the plugin will" >&2
  echo "  fail to boot until a legally-owned Monomachine OS 1.32b .bin is" >&2
  echo "  placed there. Continuing deploy anyway." >&2
fi

echo "== Stopping any running push-mm on $PUSH_IP =="
# -x (exact process-name match), not -f (full command-line match): "pkill
# -f push-mm" run as a one-shot ssh command executes as `bash -c 'pkill -f
# push-mm || true'` remotely, and that wrapping bash's own command line
# contains the literal substring "push-mm" -- pkill -f can match and kill
# that shell itself before it reaches "|| true", dropping the ssh session
# and aborting this script (deploy.sh's set -e) with no further output.
# -x matches only the actual push-mm binary's process name, never the
# shell invoking pkill.
#
# Then WAIT for it to actually exit, not just be signaled -- pkill sends
# SIGTERM and returns immediately, before the target necessarily finished
# exiting and released its own binary's file handle. The next step scp's
# over that same binary; losing this race means "Text file busy" (seen
# for real on push-xenia's own deploy.sh). Poll for up to ~4s before
# giving up and proceeding anyway.
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  'pkill -x push-mm 2>/dev/null; for i in $(seq 1 20); do pgrep -x push-mm >/dev/null || exit 0; sleep 0.2; done; exit 0' || true

echo "== Copying to $PUSH_IP:$REMOTE_DIR =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "mkdir -p $REMOTE_DIR/module/roms"
scp "${SSH_OPTS[@]}" "$DIST_DIR/dsp.so" "root@$PUSH_IP:$REMOTE_DIR/"
scp "${SSH_OPTS[@]}" "$DIST_DIR/push-mm" "root@$PUSH_IP:$REMOTE_DIR/"
scp "${SSH_OPTS[@]}" "$DIST_DIR/hack.json" "root@$PUSH_IP:$REMOTE_DIR/"
if compgen -G "$DIST_DIR/module/roms/*.bin" > /dev/null; then
  scp "${SSH_OPTS[@]}" "$DIST_DIR/module/roms/"*.bin "root@$PUSH_IP:$REMOTE_DIR/module/roms/"
fi

echo "== Ensuring /lib64 -> /lib symlink exists (see docs/environment-and-deploy.md) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "[ -e /lib64 ] || ln -s /lib /lib64"

echo "== Relaunching push-mm on $PUSH_IP =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  "cd $REMOTE_DIR && chmod +x push-mm && nohup ./push-mm -config hack.json > push-mm.log 2>&1 &"

echo "Deployed and (re)launched. Logs: ssh ${SSH_OPTS[*]} root@$PUSH_IP tail -f $REMOTE_DIR/push-mm.log"
