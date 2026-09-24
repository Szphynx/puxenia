#!/usr/bin/env bash
# build.sh — builds push-sample-fetch (Go, CGO_ENABLED=0 so it's fully
# static — no glibc-version concerns the way the cgo pieces elsewhere in
# this repo have, see docs/environment-and-deploy.md) and stages a
# self-contained dist/ with the Go binary plus static yt-dlp/ffmpeg
# binaries this tool shells out to at runtime.
#
# yt-dlp/ffmpeg are downloaded once into .cache/ (gitignored, never
# committed — same spirit as push-hack-mm's ROM: a third-party binary this
# repo doesn't ship) and reused on subsequent builds. Both downloads are
# the standalone x86_64 Linux builds with no external deps, matching Push
# 3's confirmed x86_64 architecture and minimal rootfs (no python3, no apt
# packages assumed present).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST_DIR="$SCRIPT_DIR/dist"
CACHE_DIR="$SCRIPT_DIR/.cache"

YTDLP_URL="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux"
# John Van Sickle's static ffmpeg builds — widely used for exactly this
# ("a static ffmpeg binary with zero shared-lib dependencies").
FFMPEG_TARBALL_URL="https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz"

mkdir -p "$CACHE_DIR" "$DIST_DIR"

echo "== [1/3] Building push-sample-fetch (Go, static) =="
( cd "$SCRIPT_DIR/src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$DIST_DIR/push-sample-fetch" . )

echo "== [2/3] Fetching yt-dlp (cached in .cache/) =="
if [ ! -x "$CACHE_DIR/yt-dlp" ]; then
  curl -fL -o "$CACHE_DIR/yt-dlp" "$YTDLP_URL"
  chmod +x "$CACHE_DIR/yt-dlp"
fi
cp "$CACHE_DIR/yt-dlp" "$DIST_DIR/yt-dlp"

echo "== [3/3] Fetching static ffmpeg (cached in .cache/) =="
if [ ! -x "$CACHE_DIR/ffmpeg" ]; then
  curl -fL -o "$CACHE_DIR/ffmpeg-release-amd64-static.tar.xz" "$FFMPEG_TARBALL_URL"
  tar -C "$CACHE_DIR" -xJf "$CACHE_DIR/ffmpeg-release-amd64-static.tar.xz"
  # extracted dir is named ffmpeg-<version>-amd64-static/ — grab the binary
  # out of whichever version dir just appeared, without hardcoding a version.
  found="$(find "$CACHE_DIR" -maxdepth 2 -type f -name ffmpeg -path '*-amd64-static/*' | head -1)"
  if [ -z "$found" ]; then
    echo "error: couldn't find ffmpeg binary inside the extracted tarball" >&2
    exit 1
  fi
  cp "$found" "$CACHE_DIR/ffmpeg"
  chmod +x "$CACHE_DIR/ffmpeg"
fi
cp "$CACHE_DIR/ffmpeg" "$DIST_DIR/ffmpeg"

cp "$SCRIPT_DIR/hack.json" "$DIST_DIR/hack.json"

cat <<EOF

Build complete. Staged at: $DIST_DIR
  $DIST_DIR/push-sample-fetch  (binary)
  $DIST_DIR/yt-dlp
  $DIST_DIR/ffmpeg
  $DIST_DIR/hack.json

Run locally (e.g. on Push 3 itself over SSH) with:
  cd $DIST_DIR && SAMPLE_DIR=/path/confirmed/on/your/push ./push-sample-fetch

SAMPLE_DIR has no safe default — see README.md's "finding your Push's
sample folder" section before running this for real.

Or deploy to a Push 3 over SSH with: ./deploy.sh <push-ip> [ssh-key] [sample-dir]
EOF
