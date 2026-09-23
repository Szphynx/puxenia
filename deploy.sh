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
#
# Then WAIT for it to actually be gone, not just signaled -- pkill sends
# SIGTERM and returns immediately, before the target has necessarily
# finished exiting and released its own binary's file handle. The very
# next step scp's over that same binary, and a real run hit exactly this:
# `scp: /tmp/xenia-hack/push-xenia: Text file busy`, aborting the deploy
# right after a full rebuild. Poll for up to ~4s (20 * 0.2s) before giving
# up and proceeding anyway -- best-effort, not a hard guarantee, but far
# less likely to lose the race than proceeding immediately.
ssh_ 'pkill -x push-xenia 2>/dev/null; for i in $(seq 1 20); do pgrep -x push-xenia >/dev/null || exit 0; sleep 0.2; done; exit 0' || true

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

# Shared with deploy-all.sh (which calls this regardless of which hacks'
# flags are passed, so a --mm --hub-only run doesn't silently skip it) --
# see ensure-audio-loopback.sh's own doc for why this moved out of being
# Xenia-exclusive.
source "$(dirname "${BASH_SOURCE[0]}")/ensure-audio-loopback.sh"
ensure_audio_loopback

echo "==> Clearing any stuck push-hub loading splash for xenia (best-effort)"
# push-hub isn't touched by this redeploy at all (plain scp + pkill +
# relaunch, never through push-hub's own START/STOP), so a stuck splash
# from before this redeploy would otherwise sit there until push-hub's
# own alive-poll or splashTimeout catches up (push-hub/src/splash.go's
# doc) -- this is immediate instead. Best-effort: push-hub may not be
# installed/running at all (a solo Xenia-only checkout), so a failure
# here must never fail the deploy.
ssh_ "curl -fsS -m 2 -X POST 'http://localhost:7709/api/splash/clear?id=xenia' >/dev/null 2>&1 || true" || true

if [[ "${BACKGROUND:-}" == "1" ]]; then
    echo "==> Launching push-xenia detached (BACKGROUND=1)"
    ssh_ "cd $REMOTE_DIR && chmod +x push-xenia && nohup ./push-xenia > push-xenia.log 2>&1 &"
    echo "Deployed and (re)launched. Logs: ssh -i $PUSH_KEY root@${PUSH_HOST} tail -f $REMOTE_DIR/push-xenia.log"
else
    echo "==> Launching push-xenia (Ctrl+C to stop)"
    ssh -t -i "$PUSH_KEY" "root@${PUSH_HOST}" "cd $REMOTE_DIR && chmod +x push-xenia && ./push-xenia"
fi
