#!/usr/bin/env bash
# deploy.sh -- rebuild push-xenia + libxenia-plugin.so and redeploy to Push,
# redoing everything /tmp's tmpfs wipe (and a Push reboot) throws away:
# the ROM, the /lib64 symlink Push's rootfs doesn't ship, and the
# snd-aloop kernel module. Safe to re-run after every reboot or every
# code change -- idempotent where it can be, harmless to repeat where
# it can't (e.g. re-copying the ROM).
#
# Usage: ./deploy.sh [push-host] [ssh-key-path]
#   Both optional, same positional-args-with-env-fallback shape as
#   push-hack-mm/deploy.sh and push-hub/deploy.sh (deploy-all.sh calls
#   all three the same way) -- added after a real mix-up: this script used
#   to be env-var-only, so passing an IP as $1 silently did nothing and it
#   fell back to the hardcoded default host instead, with no error.
# Configure via env vars (defaults match this project's own setup):
#   PUSH_HOST=192.168.3.89             (overridden by $1 if given)
#   PUSH_KEY=~/xenia-build/pushkey     (overridden by $2 if given)
#   ROM_DIR=~/xenia-build/rom          (local dir with the .bin ROM files)
#   REPO_DIR=~/puxenia                 (this repo's checkout)
#   NO_BUILD=1                         (skip the build step, just redeploy)
#   BACKGROUND=1                       (launch detached via nohup + log file,
#                                        same as push-hack-mm/push-hub's own
#                                        deploy.sh, instead of the default
#                                        interactive `ssh -t` foreground
#                                        session -- used by deploy-all.sh so
#                                        Xenia doesn't block the other hacks'
#                                        deploys; the default stays
#                                        interactive since that's this
#                                        project's own established solo
#                                        workflow)
set -euo pipefail

# go.mod needs a real 1.25+ toolchain; apt's golang-go (1.18) doesn't cut
# it and this script shouldn't depend on ~/.bashrc having been sourced.
[[ -x /usr/local/go/bin/go ]] && PATH="/usr/local/go/bin:$PATH"

PUSH_HOST="${1:-${PUSH_HOST:-192.168.3.89}}"
PUSH_KEY="${2:-${PUSH_KEY:-$HOME/xenia-build/pushkey}}"
ROM_DIR="${ROM_DIR:-$HOME/xenia-build/rom}"
REPO_DIR="${REPO_DIR:-$HOME/puxenia}"
ALOOP_KO_DIR="${ALOOP_KO_DIR:-$HOME/push-hack-audio-loopback/ko}"
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
# -x (exact process-name match), not -f: -f matches full command lines, and
# a one-shot ssh command executes remotely as `bash -c 'pkill -f push-xenia
# || true'` -- that wrapping bash's own command line contains the literal
# substring "push-xenia", so pkill -f can match and kill it before "||
# true" ever runs, dropping the ssh session and aborting this script (set
# -e) with no further output. Same bug, same fix, as push-hack-mm/deploy.sh.
ssh_ "pkill -x push-xenia" || true

echo "==> Ensuring remote directories exist"
ssh_ "mkdir -p '$REMOTE_DIR/module' '$REMOTE_DIR/ui'"

echo "==> Copying binary + plugin + config"
scp_ "$REPO_DIR/xenia-plugin/build/libxenia-plugin.so" "root@${PUSH_HOST}:$REMOTE_DIR/dsp.so"
scp_ "$REPO_DIR/push-hack-xenia/src/push-xenia" "root@${PUSH_HOST}:$REMOTE_DIR/"
scp_ "$REPO_DIR/push-hack-xenia/hack.json" "root@${PUSH_HOST}:$REMOTE_DIR/"
scp_ -r "$REPO_DIR/push-hack-xenia/src/ui"/* "root@${PUSH_HOST}:$REMOTE_DIR/ui/"

echo "==> Copying ROM (wiped by every reboot along with the rest of /tmp)"
# xenia_create_instance chdir()s to <module_dir>/roms before loading -- NOT
# module_dir itself. Copying flat into module/ (as this used to do) means
# that chdir fails, xenia_create_instance sets bootFailed, and
# xenia_render_block silently memsets its output to zero forever -- with
# bridge_plugin_load never checking for that failure, the Go host prints
# "plugin loaded and instance created" and runs completely normally,
# producing perfect digital silence. This was the real cause of every
# "notes come in but no audio" session, independent of anything about
# MIDI/device/channel routing.
ssh_ "mkdir -p '$REMOTE_DIR/module/roms'"
if [[ -d "$ROM_DIR" ]] && compgen -G "$ROM_DIR/*.BIN" > /dev/null || compgen -G "$ROM_DIR/*.bin" > /dev/null 2>&1; then
    scp_ "$ROM_DIR"/*.BIN "root@${PUSH_HOST}:$REMOTE_DIR/module/roms/" 2>/dev/null || \
    scp_ "$ROM_DIR"/*.bin "root@${PUSH_HOST}:$REMOTE_DIR/module/roms/"
else
    echo "   WARNING: no .bin/.BIN files found in $ROM_DIR -- set ROM_DIR if this is wrong."
fi

echo "==> Fixing up things a Push reboot resets"
ssh_ "[ -e /lib64 ] || ln -s /lib /lib64"

# push-xenia looks for card id "Audio" -- NOT the "PHVAudio" driver
# name shown in /proc/asound/cards's second column. That id is ALSA's
# auto-derived id for this module's longname ("Push Hack Virtual
# Audio"), since it's insmod'd with no id= override. Don't try to force
# id=PHVAudio here: once Live has opened the card's PCM devices, an
# id= reload requires rmmod first, which fails EBUSY while Live holds
# it open -- forcing that would mean killing Live just to rename an
# already-working card.
if ! ssh_ "grep -qE '^\s*[0-9]+ \[Audio *\]' /proc/asound/cards 2>/dev/null"; then
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

if [[ "${BACKGROUND:-}" == "1" ]]; then
    echo "==> Launching push-xenia detached (BACKGROUND=1)"
    ssh_ "cd $REMOTE_DIR && chmod +x push-xenia && nohup ./push-xenia > push-xenia.log 2>&1 &"
    echo "Deployed and (re)launched. Logs: ssh -i $PUSH_KEY root@${PUSH_HOST} tail -f $REMOTE_DIR/push-xenia.log"
else
    echo "==> Launching push-xenia (Ctrl+C to stop)"
    ssh -t -i "$PUSH_KEY" "root@${PUSH_HOST}" "cd $REMOTE_DIR && chmod +x push-xenia && ./push-xenia"
fi
