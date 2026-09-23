#!/usr/bin/env bash
# pull_deploy.sh — the day-to-day loop for puMMa: pull the latest commits,
# rebuild mm-plugin + push-mm, and redeploy to a Push 3 over SSH.
# Same shape as the manual puXenia routine (git pull, cmake --build,
# kill+scp+relaunch over ssh), just scripted end to end via this hack's
# own build.sh/deploy.sh (see those + install_mm_push.sh for first-time
# setup, which this assumes is already done).
#
# Usage: ./pull_deploy.sh [push-ip] [ssh-key-path] [git-branch]
#   PUSH_IP / PUSH_KEY / PULL_BRANCH env vars work too.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PUSH_IP="${1:-${PUSH_IP:-192.168.3.89}}"
PUSH_KEY="${2:-${PUSH_KEY:-$HOME/xenia-build/pushkey}}"
PULL_BRANCH="${3:-${PULL_BRANCH:-main}}"

echo "== [1/2] git pull origin $PULL_BRANCH =="
( cd "$REPO_ROOT" && git pull origin "$PULL_BRANCH" )

echo "== [2/2] build + deploy + relaunch on $PUSH_IP =="
"$SCRIPT_DIR/deploy.sh" "$PUSH_IP" "$PUSH_KEY"
