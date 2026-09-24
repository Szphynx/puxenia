#!/usr/bin/env bash
# discover-sample-dir.sh — read-only helper that SSHes into a real Push 3
# and surfaces CANDIDATE sample-library directories for a human to
# confirm. Writes nothing, changes nothing. This exists because
# push-sample-fetch itself refuses to guess SAMPLE_DIR (see README.md) —
# this script automates the manual find/lsof commands the README
# describes, so you don't have to type them by hand.
#
# Usage: ./discover-sample-dir.sh <push-ip> [ssh-key-path]
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

echo "Connecting to root@$PUSH_IP ..."
echo

ssh "${SSH_OPTS[@]}" "root@$PUSH_IP" bash <<'REMOTE'
set -uo pipefail

echo "== 1. Any process that looks like Ableton Live =="
pgrep -fla -i live || echo "(nothing matching 'live' found — Live may be named differently in ps, or not currently running)"
echo

echo "== 2. If found above: files it currently has open that look like samples =="
for pid in $(pgrep -f -i live 2>/dev/null || true); do
  echo "--- pid $pid ---"
  echo "cwd: $(readlink -f "/proc/$pid/cwd" 2>/dev/null || echo unknown)"
  if command -v lsof >/dev/null 2>&1; then
    lsof -p "$pid" 2>/dev/null | grep -iE '\.wav|\.aif|sample|librar' || echo "  (lsof found nothing sample-related for this pid)"
  else
    echo "  (no lsof on this system — skipping open-file check; /proc/$pid/fd is a manual fallback)"
  fi
done
echo

echo "== 3. Directories anywhere on disk holding the most .wav/.aif/.aiff files =="
echo "   (top 20 by file count, likely-factory/read-only paths filtered out — verify"
echo "   before trusting: a real user library is normally where you can also see"
echo "   OWN recordings/renders, not just factory content)"
find / \( -iname '*.wav' -o -iname '*.aif' -o -iname '*.aiff' \) 2>/dev/null \
  | grep -viE '/(factory|core[-_]?library|packs?)/' \
  | xargs -n1 dirname 2>/dev/null \
  | sort | uniq -c | sort -rn | head -20
echo

echo "== 4. Any directory literally named like 'User Library' =="
find / -iname "*user*librar*" -type d 2>/dev/null
echo

echo "== 5. Disk mounts (in case the library lives on a separate partition) =="
df -h 2>/dev/null | grep -vE '^tmpfs|^overlay|^devtmpfs'
echo

echo "Done. Cross-check these against README.md's guidance before picking one —"
echo "this is a best-effort scan, not a confirmed answer. Once you're confident,"
echo "set SAMPLE_DIR to it and deploy.sh."
REMOTE
