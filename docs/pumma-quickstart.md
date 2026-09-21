# puMMa quickstart: first-time setup, build, deploy

For `push-hack-mm` (puMMa, Elektron Monomachine port). Mirrors the
`puXenia` deploy loop you already use, plus the one-time setup puMMa
needs on top of that (a `gearmulator-md-mm` checkout and your own ROM).
See `docs/monomachine-port-notes.md` for the architecture and
`docs/environment-and-deploy.md` for WSL-specific dev-machine gotchas
(old apt Go, `libasound2-dev`, etc.) if any step below fails oddly.

## Prerequisites

- Go 1.25+ (`go version`) and a C++17 toolchain + CMake, same as any
  other hack in this repo.
- `libasound2-dev` (cgo/ALSA headers).
- Your own legally-owned Elektron SFX6-60 OS 1.32b ROM (`.bin`, exactly
  8MiB). Not included in this repo — puMMa won't boot without it.
- SSH access to your Push 3 (`root@<push-ip>`, key-based).

## First run: one command

```
cd ~/puxenia/push-hack-mm
./install_mm_push.sh 192.168.3.89 ~/xenia-build/pushkey ~/path/to/your-mm-os1.32b.bin
```

This does everything, start to finish:

1. Clones `joelanders/gearmulator-md-mm` next to this repo (as a sibling
   directory: `~/joelanders/gearmulator-md-mm`) if it isn't there yet,
   and pulls just the submodules the headless build needs
   (`dsp56300` + its nested `asmjit`, `mc68k` — *not* the JUCE/RmlUi
   submodules, those are only for the full plugin build).
2. Installs `libasound2-dev` if missing (asks for `sudo`).
3. Stages your ROM into `push-hack-mm/dist/module/roms/`.
4. Builds `mm-plugin` (CMake) and `push-mm` (Go), stages `dist/`, kills
   any running `push-mm` on the Push, copies everything over SSH, and
   relaunches it.

All three arguments are optional after the first run — they fall back to
env vars (`PUSH_IP` / `PUSH_KEY` / `MM_ROM`) then this repo's own
defaults (`192.168.3.89`, `~/xenia-build/pushkey`). Once the ROM is
staged you don't need to pass it again:

```
./install_mm_push.sh
```

## Verify it's running

```
ssh -i ~/xenia-build/pushkey root@192.168.3.89 tail -f /tmp/mm-hack/push-mm.log
```

On Push: **Shift + Device** toggles puMMa's on-screen UI. Give it ~20-22
seconds after launch before pressing pads — the emulated Monomachine
firmware genuinely ignores all input while it boots (see
`docs/monomachine-port-notes.md`'s boot-gate section); the web UI's
status badge shows "Booting…" during that window if the on-screen one
doesn't reach you fast enough. Web UI: `http://192.168.3.89:7708/`.

## Day-to-day: pull latest + rebuild + redeploy

Same shape as your manual puXenia routine (`git pull`, `cmake --build`,
kill+scp+relaunch over ssh), scripted as one command:

```
cd ~/puxenia/push-hack-mm
./pull_deploy.sh
```

Equivalent to, and replaces, typing:

```
cd ~/puxenia && git pull origin main
cd push-hack-mm && ./deploy.sh 192.168.3.89 ~/xenia-build/pushkey
```

Use `./deploy.sh` directly (skip the `git pull`) if you only changed
local, uncommitted code and just want to rebuild + redeploy.

## Stopping puMMa (e.g. to try a different hack)

```
cd ~/puxenia/push-hack-mm
./stop.sh 192.168.3.89 ~/xenia-build/pushkey
```

Kills puMMa cleanly and resets push-manager's display/MIDI-filter state
directly (not just relying on puMMa's own shutdown handler), so
Shift+Device and the audio device are immediately free for another hack
— use this instead of manually hunting down the process.

## Manual build only (no deploy)

```
cd ~/puxenia/push-hack-mm
./build.sh
```

Stages everything into `push-hack-mm/dist/` without touching the Push —
useful for checking the build compiles before shipping it.
