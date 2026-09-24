#!/usr/bin/env bash
# deploy.sh — builds (via build.sh) then ships dist/ to a Push 3 over SSH
# and relaunches. Mirrors the other hacks' deploy.sh (kill-first to avoid
# "Text file busy", /lib64 symlink workaround — see
# docs/environment-and-deploy.md), adapted for a plain HTTP service with no
# push-manager/push-display/ALSA dependency at all.
#
# Usage: ./deploy.sh <push-ip> [ssh-key-path] <sample-dir> [remote-dir]
#   PUSH_IP / PUSH_KEY / SAMPLE_DIR / PUSH_REMOTE_DIR env vars work too.
#
# <sample-dir> (or SAMPLE_DIR) is REQUIRED and has no default — it's the
# absolute path, ON PUSH ITSELF, that Ableton Live's own User Library
# browser watches for samples. This script does not and cannot guess it.
# See README.md's "finding your Push's sample folder" section if you
# haven't confirmed it yet.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"

PUSH_IP="${1:-${PUSH_IP:-}}"
PUSH_KEY="${2:-${PUSH_KEY:-}}"
SAMPLE_DIR="${3:-${SAMPLE_DIR:-}}"
REMOTE_DIR="${4:-${PUSH_REMOTE_DIR:-/tmp/sample-fetch-hack}}"

if [ -z "$PUSH_IP" ] || [ -z "$SAMPLE_DIR" ]; then
  echo "usage: $0 <push-ip> [ssh-key-path] <sample-dir> [remote-dir]" >&2
  echo "   (or set PUSH_IP / PUSH_KEY / SAMPLE_DIR / PUSH_REMOTE_DIR)" >&2
  echo "   sample-dir is REQUIRED -- see README.md, this is not guessable." >&2
  exit 1
fi

SSH_OPTS=(-o StrictHostKeyChecking=accept-new)
if [ -n "$PUSH_KEY" ]; then
  SSH_OPTS+=(-i "$PUSH_KEY")
fi

"$SCRIPT_DIR/build.sh"

echo "== Stopping any running push-sample-fetch on $PUSH_IP =="
# -x (exact process-name match), not -f -- see push-hack-mm/deploy.sh's own
# comment on why -f is unsafe here (it can match and kill the wrapping ssh
# shell itself). Then wait for actual exit before scp'ing over the binary.
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  'pkill -x push-sample-fetch 2>/dev/null; for i in $(seq 1 20); do pgrep -x push-sample-fetch >/dev/null || exit 0; sleep 0.2; done; exit 0' || true

echo "== Copying to $PUSH_IP:$REMOTE_DIR =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "mkdir -p $REMOTE_DIR $SAMPLE_DIR"
scp "${SSH_OPTS[@]}" "$DIST_DIR/push-sample-fetch" "$DIST_DIR/yt-dlp" "$DIST_DIR/ffmpeg" "$DIST_DIR/hack.json" \
  "root@$PUSH_IP:$REMOTE_DIR/"

echo "== Ensuring /lib64 -> /lib symlink exists (yt-dlp's PyInstaller build expects it; see docs/environment-and-deploy.md) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" "[ -e /lib64 ] || ln -s /lib /lib64"

echo "== Relaunching push-sample-fetch on $PUSH_IP (sample-dir=$SAMPLE_DIR) =="
ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" \
  "cd $REMOTE_DIR && chmod +x push-sample-fetch yt-dlp ffmpeg && \
   SAMPLE_DIR='$SAMPLE_DIR' nohup ./push-sample-fetch -sample-dir '$SAMPLE_DIR' -ytdlp ./yt-dlp -ffmpeg ./ffmpeg \
   > push-sample-fetch.log 2>&1 &"

echo "Deployed and (re)launched. Logs: ssh ${SSH_OPTS[*]} root@$PUSH_IP tail -f $REMOTE_DIR/push-sample-fetch.log"
echo "Open from any browser on the same wifi as Push: http://$PUSH_IP:7710/"
