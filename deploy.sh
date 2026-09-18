#!/usr/bin/env bash
# deploy.sh -- rebuild push-xenia + libxenia-plugin.so and redeploy to Push,
# redoing everything /tmp's tmpfs wipe (and a Push reboot) throws away:
# the ROM, the /lib64 symlink Push's rootfs doesn't ship, and the
# snd-aloop kernel module. Safe to re-run after every reboot or every
# code change -- idempotent where it can be, harmless to repeat where
# it can't (e.g. re-copying the ROM).
#
# Usage: ./deploy.sh
# Configure via env vars (defaults match this project's own setup):
#   PUSH_HOST=192.168.3.89
#   PUSH_KEY=~/xenia-build/pushkey
#   ROM_DIR=~/xenia-build/rom          (local dir with the .bin ROM files)
#   REPO_DIR=~/puxenia                 (this repo's checkout)
#   NO_BUILD=1                         (skip the build step, just redeploy)
set -euo pipefail

PUSH_HOST="${PUSH_HOST:-192.168.3.89}"
PUSH_KEY="${PUSH_KEY:-$HOME/xenia-build/pushkey}"
ROM_DIR="${ROM_DIR:-$HOME/xenia-build/rom}"
REPO_DIR="${REPO_DIR:-$HOME/puxenia}"
ALOOP_KO_DIR="${ALOOP_KO_DIR:-$HOME/federico-pepe/push-hack-audio-loopback/ko}"
REMOTE_DIR="/tmp/xenia-hack"

ssh_() { ssh -i "$PUSH_KEY" "root@${PUSH_HOST}" "$@"; }
scp_() { scp -i "$PUSH_KEY" "$@"; }

echo "==> Building"
if [[ "${NO_BUILD:-}" != "1" ]]; then
    cmake --build "$REPO_DIR/xenia-plugin/build" -j"$(nproc)"
    ( cd "$REPO_DIR/push-hack-xenia/src" && go build -o push-xenia . )
else
    echo "   (skipped, NO_BUILD=1)"
fi

echo "==> Stopping any running push-xenia on Push"
ssh_ "pkill -f push-xenia" || true

echo "==> Ensuring remote directories exist"
ssh_ "mkdir -p '$REMOTE_DIR/module' '$REMOTE_DIR/ui'"

echo "==> Copying binary + plugin + config"
scp_ "$REPO_DIR/xenia-plugin/build/libxenia-plugin.so" "root@${PUSH_HOST}:$REMOTE_DIR/"
scp_ "$REPO_DIR/push-hack-xenia/src/push-xenia" "root@${PUSH_HOST}:$REMOTE_DIR/"
scp_ "$REPO_DIR/push-hack-xenia/hack.json" "root@${PUSH_HOST}:$REMOTE_DIR/"
scp_ -r "$REPO_DIR/push-hack-xenia/src/ui"/* "root@${PUSH_HOST}:$REMOTE_DIR/ui/"

echo "==> Copying ROM (wiped by every reboot along with the rest of /tmp)"
if [[ -d "$ROM_DIR" ]] && compgen -G "$ROM_DIR/*.BIN" > /dev/null || compgen -G "$ROM_DIR/*.bin" > /dev/null 2>&1; then
    scp_ "$ROM_DIR"/*.BIN "root@${PUSH_HOST}:$REMOTE_DIR/module/" 2>/dev/null || \
    scp_ "$ROM_DIR"/*.bin "root@${PUSH_HOST}:$REMOTE_DIR/module/"
else
    echo "   WARNING: no .bin/.BIN files found in $ROM_DIR -- set ROM_DIR if this is wrong."
fi

echo "==> Fixing up things a Push reboot resets"
ssh_ "[ -e /lib64 ] || ln -s /lib /lib64"

if ! ssh_ "lsmod | grep -q snd_aloop"; then
    KVER="$(ssh_ "uname -r")"
    KO_LOCAL="$ALOOP_KO_DIR/$KVER/snd-aloop.ko"
    if [[ -f "$KO_LOCAL" ]]; then
        echo "   Loading bundled snd-aloop.ko for kernel $KVER (push-hack-audio-loopback"
        echo "   was never installed persistently via the catalog, so this is redone"
        echo "   on every reboot -- see push-hack-audio-loopback/README.md to install"
        echo "   it properly instead)."
        scp_ "$KO_LOCAL" "root@${PUSH_HOST}:$REMOTE_DIR/snd-aloop.ko"
        ssh_ "insmod $REMOTE_DIR/snd-aloop.ko"
    else
        echo "   WARNING: no bundled snd-aloop.ko for kernel $KVER in $ALOOP_KO_DIR --"
        echo "   audio loopback will not work. See push-hack-audio-loopback/README.md"
        echo "   to build one for this kernel."
    fi
fi

echo "==> Launching push-xenia (Ctrl+C to stop)"
ssh -t -i "$PUSH_KEY" "root@${PUSH_HOST}" "cd $REMOTE_DIR && chmod +x push-xenia && ./push-xenia"
