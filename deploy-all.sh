#!/usr/bin/env bash
# deploy-all.sh -- one command for all three Push 3 hacks in this repo
# (puXenia, puMMa, push-hub), with flags to install just one or two of
# them instead of always rebuilding everything. Each hack keeps its own
# build.sh/deploy.sh untouched -- this only decides which of them to run
# and pulls latest source once up front, it doesn't duplicate their logic.
#
# Usage:
#   ./deploy-all.sh                  # all three (default, same as --all)
#   ./deploy-all.sh --xenia          # just puXenia
#   ./deploy-all.sh --mm             # just puMMa
#   ./deploy-all.sh --hub            # just push-hub
#   ./deploy-all.sh --xenia --mm     # any combination
#
# Configure via env vars (same names/defaults as the per-hack scripts):
#   PUSH_IP=192.168.3.89
#   PUSH_KEY=~/xenia-build/pushkey
#   PULL_BRANCH=main                 (branch puMMa's own deploy pulls)
#   NO_PULL=1                        (skip the repo-wide git pull)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat <<EOF
Usage: $0 [--xenia] [--mm] [--hub] [--all]
  No flags = --all (install/redeploy all three).
  Any combination of --xenia/--mm/--hub installs just those.
Env vars: PUSH_IP, PUSH_KEY, PULL_BRANCH, NO_PULL=1
EOF
}

DO_XENIA=0
DO_MM=0
DO_HUB=0
for arg in "$@"; do
    case "$arg" in
        --xenia) DO_XENIA=1 ;;
        --mm) DO_MM=1 ;;
        --hub) DO_HUB=1 ;;
        --all) DO_XENIA=1; DO_MM=1; DO_HUB=1 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown flag: $arg" >&2; usage; exit 1 ;;
    esac
done
# No flags at all -> install all three (this script's own default, per
# its own name/purpose -- not the same as "--all" being required).
if [[ $DO_XENIA -eq 0 && $DO_MM -eq 0 && $DO_HUB -eq 0 ]]; then
    DO_XENIA=1; DO_MM=1; DO_HUB=1
fi

PUSH_IP="${PUSH_IP:-192.168.3.89}"
PUSH_KEY="${PUSH_KEY:-$HOME/xenia-build/pushkey}"
PULL_BRANCH="${PULL_BRANCH:-main}"

if [[ "${NO_PULL:-}" != "1" ]]; then
    echo "== git pull origin $PULL_BRANCH =="
    ( cd "$SCRIPT_DIR" && git pull origin "$PULL_BRANCH" )
else
    echo "== skipping git pull (NO_PULL=1) =="
fi

if [[ $DO_XENIA -eq 1 ]]; then
    echo
    echo "== puXenia =="
    # BACKGROUND=1: deploy.sh's own default launches push-xenia
    # interactively (ssh -t, Ctrl+C to stop) for solo manual use -- that
    # would block this script forever before it ever reaches puMMa/
    # push-hub, so run it detached here instead, same as the other two
    # hacks already do.
    ( cd "$SCRIPT_DIR" && PUSH_HOST="$PUSH_IP" PUSH_KEY="$PUSH_KEY" BACKGROUND=1 ./deploy.sh )
fi

if [[ $DO_MM -eq 1 ]]; then
    echo
    echo "== puMMa =="
    ( cd "$SCRIPT_DIR/push-hack-mm" && ./deploy.sh "$PUSH_IP" "$PUSH_KEY" )
fi

if [[ $DO_HUB -eq 1 ]]; then
    echo
    echo "== push-hub =="
    ( cd "$SCRIPT_DIR/push-hub" && ./deploy.sh "$PUSH_IP" "$PUSH_KEY" )
fi

SELECTED=""
if [[ $DO_XENIA -eq 1 ]]; then SELECTED="$SELECTED xenia"; fi
if [[ $DO_MM -eq 1 ]]; then SELECTED="$SELECTED mm"; fi
if [[ $DO_HUB -eq 1 ]]; then SELECTED="$SELECTED hub"; fi
echo
echo "Done. Selected:$SELECTED"
