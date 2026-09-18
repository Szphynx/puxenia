#!/usr/bin/env bash
# build.sh — builds mm-plugin (C++) and push-mm (Go) and stages everything
# push-catalog/a manual copy needs into dist/. Mirrors the manual steps in
# docs/environment-and-deploy.md's "Full deploy loop" (written for Xenia,
# same shape here) — this script exists so that loop doesn't need to be
# retyped by hand every time.
#
# Usage: ./build.sh [Release|Debug]
set -euo pipefail

BUILD_TYPE="${1:-Release}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
PLUGIN_DIR="$REPO_ROOT/mm-plugin"
PLUGIN_BUILD_DIR="$PLUGIN_DIR/build"
DIST_DIR="$SCRIPT_DIR/dist"

# GEARMULATOR_MD_MM_SOURCE: where the joelanders/gearmulator-md-mm checkout
# lives — mm-plugin/CMakeLists.txt defaults to a sibling
# ../../joelanders/gearmulator-md-mm relative to mm-plugin/ (i.e.
# <repo-root-parent>/joelanders/gearmulator-md-mm), matching how this
# session cloned it. Override by exporting GEARMULATOR_MD_MM_SOURCE
# yourself before running this script if your checkout lives elsewhere.
if [ -n "${GEARMULATOR_MD_MM_SOURCE:-}" ]; then
  CMAKE_SRC_ARG="-DGEARMULATOR_MD_MM_SOURCE=$GEARMULATOR_MD_MM_SOURCE"
else
  CMAKE_SRC_ARG=""
fi

echo "== [1/3] Configuring + building mm-plugin ($BUILD_TYPE) =="
cmake -S "$PLUGIN_DIR" -B "$PLUGIN_BUILD_DIR" \
  -DCMAKE_BUILD_TYPE="$BUILD_TYPE" $CMAKE_SRC_ARG
cmake --build "$PLUGIN_BUILD_DIR" -j"$(nproc)"

PLUGIN_SO="$PLUGIN_BUILD_DIR/libmm-plugin.so"
if [ ! -f "$PLUGIN_SO" ]; then
  echo "error: expected $PLUGIN_SO after build, not found" >&2
  exit 1
fi

echo "== [2/3] Building push-mm (Go) =="
( cd "$SCRIPT_DIR/src" && go build -o "$SCRIPT_DIR/push-mm" . )

echo "== [3/3] Staging dist/ =="
mkdir -p "$DIST_DIR/module/roms"
cp "$PLUGIN_SO" "$DIST_DIR/dsp.so"
cp "$SCRIPT_DIR/push-mm" "$DIST_DIR/push-mm"
cp "$SCRIPT_DIR/hack.json" "$DIST_DIR/hack.json"
# push-mm's own on-screen UI (ui/index.html) is compiled INTO the binary
# via Go's //go:embed — nothing else needs to be staged for it.

cat <<EOF

Build complete. Staged at: $DIST_DIR
  $DIST_DIR/push-mm     (binary)
  $DIST_DIR/dsp.so      (mm-plugin)
  $DIST_DIR/hack.json
  $DIST_DIR/module/roms/  <- put your legally-owned Monomachine OS 1.32b
                             ROM (.bin, 8MiB) here before deploying/running.

Run locally (e.g. on Push 3 itself, or any Linux box with push-manager +
push-audio-loopback already running) with:
  cd $DIST_DIR && ./push-mm -config hack.json

Or deploy to a Push 3 over SSH with: ./deploy.sh <push-ip>
EOF
