#!/usr/bin/env bash
# install_mm_push.sh — one command, start to finish. Makes sure this dev
# machine actually has what mm-plugin's build needs (a gearmulator-md-mm
# checkout with the right submodules, libasound2-dev), stages your ROM,
# then hands off to build.sh/deploy.sh (already-working scripts — this
# just removes the manual setup steps in front of them, the same
# multi-command sequence documented in docs/monomachine-port-notes.md and
# walked through in chat).
#
# Usage:
#   ./install_mm_push.sh [push-ip] [ssh-key-path] [rom-path]
#
# All three are optional and fall back, in order, to: the matching env
# var (PUSH_IP / PUSH_KEY / MM_ROM), then this project's own dev-machine
# defaults below (same values used in this repo's own deploy walkthrough)
# — override whichever ones don't match your setup.
#
# First run: pass the ROM path so it gets staged.
#   ./install_mm_push.sh 192.168.3.89 ~/xenia-build/pushkey ~/roms/mm-os132b.bin
# Later runs (ROM already staged, just rebuild+redeploy):
#   ./install_mm_push.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PUSH_IP="${1:-${PUSH_IP:-192.168.3.89}}"
PUSH_KEY="${2:-${PUSH_KEY:-$HOME/xenia-build/pushkey}}"
MM_ROM="${3:-${MM_ROM:-}}"

# GEARMULATOR_MD_MM_ROOT: where the joelanders/gearmulator-md-mm checkout
# lives (or will be cloned to) — a sibling of this repo, matching
# mm-plugin/CMakeLists.txt's own default. Override with an env var of the
# same name if yours lives elsewhere; GEARMULATOR_MD_MM_SOURCE (the
# "source" subdir inside it) is what actually gets passed to cmake, via
# build.sh, same as if you'd set it yourself.
GM_ROOT_DEFAULT="$(cd "$REPO_ROOT/.." && pwd)/joelanders/gearmulator-md-mm"
GM_ROOT="${GEARMULATOR_MD_MM_ROOT:-$GM_ROOT_DEFAULT}"
export GEARMULATOR_MD_MM_SOURCE="$GM_ROOT/source"

echo "== [1/4] gearmulator-md-mm dependency source =="
if [ ! -d "$GM_ROOT/.git" ]; then
  echo "  not found — cloning into $GM_ROOT"
  git clone https://github.com/joelanders/gearmulator-md-mm.git "$GM_ROOT"
else
  echo "  found at $GM_ROOT"
fi
# Deliberately NOT --recurse-submodules on the clone above: that fork also
# lists JUCE/RmlUi/freetype as submodules for its full plugin builds,
# which mm-plugin's headless CMakeLists.txt never touches — only pull the
# 2 submodules actually needed (dsp56300, mc68k) plus dsp56300's own
# nested asmjit submodule.
if [ ! -f "$GM_ROOT/source/dsp56300/source/CMakeLists.txt" ]; then
  echo "  initializing dsp56300 + mc68k submodules"
  (cd "$GM_ROOT" && git submodule update --init --depth 1 -- source/dsp56300 source/mc68k)
fi
if [ ! -f "$GM_ROOT/source/dsp56300/source/asmjit/CMakeLists.txt" ]; then
  echo "  initializing asmjit submodule"
  (cd "$GM_ROOT/source/dsp56300" && git submodule update --init --depth 1)
fi

echo "== [2/4] libasound2-dev =="
if [ -f /usr/include/alsa/asoundlib.h ]; then
  echo "  already present"
else
  echo "  installing (sudo)"
  sudo apt-get update && sudo apt-get install -y libasound2-dev
fi

echo "== [3/4] Monomachine ROM =="
DIST_ROMS="$SCRIPT_DIR/dist/module/roms"
if [ -n "$MM_ROM" ]; then
  mkdir -p "$DIST_ROMS"
  cp "$MM_ROM" "$DIST_ROMS/"
  echo "  staged: $MM_ROM -> $DIST_ROMS/"
elif compgen -G "$DIST_ROMS/*.bin" > /dev/null 2>&1; then
  echo "  already staged in $DIST_ROMS/"
else
  echo "  none staged and none passed as arg 3 — re-run with it, e.g.:"
  echo "    $0 $PUSH_IP $PUSH_KEY /path/to/your/monomachine-os1.32b.bin"
  echo "  continuing anyway; push-mm will report \"no valid ROM found\" until one is present."
fi

echo "== [4/4] Build + deploy + launch on $PUSH_IP =="
"$SCRIPT_DIR/deploy.sh" "$PUSH_IP" "$PUSH_KEY"
