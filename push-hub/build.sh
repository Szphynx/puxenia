#!/usr/bin/env bash
# build.sh — builds push-hub (Go, no DSP plugin/cgo of its own) and stages
# dist/. Mirrors push-hack-mm/build.sh's shape minus the C++ plugin step,
# which this hack doesn't have.
#
# Usage: ./build.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"

echo "== [1/2] Building push-hub (Go) =="
( cd "$SCRIPT_DIR/src" && go build -o "$SCRIPT_DIR/push-hub" . )

echo "== [2/2] Staging dist/ =="
mkdir -p "$DIST_DIR"
cp "$SCRIPT_DIR/push-hub" "$DIST_DIR/push-hub"
cp "$SCRIPT_DIR/hack.json" "$DIST_DIR/hack.json"
cp "$SCRIPT_DIR/hacks.json" "$DIST_DIR/hacks.json"

cat <<EOF

Build complete. Staged at: $DIST_DIR
  $DIST_DIR/push-hub    (binary)
  $DIST_DIR/hack.json
  $DIST_DIR/hacks.json  <- registered hacks; edit "api"/"service" per entry
                           if a hack's port or init.d service name differs,
                           and "dir"/"exec"/"process"/"log" if its deploy.sh's
                           own REMOTE_DIR/launch command/binary name differs
                           (see src/focus.go's doc on setServiceRunning)

Run locally with:
  cd $DIST_DIR && ./push-hub -config hack.json

Or deploy to a Push 3 over SSH with: ./deploy.sh <push-ip>
EOF
