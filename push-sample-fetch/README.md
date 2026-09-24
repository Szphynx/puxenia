# push-sample-fetch

Closes one specific gap: Ableton Live's own browser/Simpler UI on Push 3
has no way to fetch a sample from the internet — you paste a URL, it fetches
the audio and drops a ready `.wav` into Live's own sample library, entirely
on Push's own embedded computer (no laptop needed — just a phone or any
device on the same wifi to open the form).

Runs directly on Push 3, the same way `push-hack-xenia`/`push-hack-mm` do
(scp + nohup over SSH — see `docs/environment-and-deploy.md`). Unlike
those, it has **no MIDI, no ALSA audio, no on-screen UI, no
push-manager/push-display dependency at all** — it's a plain Go HTTP
server that shells out to `yt-dlp` and `ffmpeg`.

## Why this approach instead of "play the video and record it"

Extracting the audio track directly (`yt-dlp -x --audio-format wav`) is
what this tool does, rather than playing the video through a speaker and
re-recording it through a mic/loopback. Direct extraction is lossless
(no re-recorded background noise, no level-matching hassle, no playback
device needed at all) and is what actually happens under the hood even
in a "play and record" approach — video files already have the audio
track sitting right there. There's no reason to go through a physical
playback+capture loop when the bits are already extractable in place.

## ⚠️ Before you run this for real: confirm `SAMPLE_DIR`

**There is no default.** This tool has never been run against a real
Push 3, and this repo's own docs (see `docs/audio-pipeline-debugging.md`,
`docs/environment-and-deploy.md`) are full of examples of things that
"looked obviously right" on Push and weren't — guessing Live's own User
Library path here and shipping a wrong default would silently write
samples into a folder Live never looks at, with no error at all. Don't
trust a guessed path; confirm it.

**Fastest path**: run `./discover-sample-dir.sh <push-ip> [ssh-key]` — a
read-only helper that SSHes in and runs the searches below for you
(Live's open file handles if it's running, directories with the most
existing `.wav`/`.aif` files, anything named like "User Library"),
printing candidates to cross-check rather than writing or changing
anything.

**Or by hand**, SSH'd into Push as root:

1. From Push's own screen (or Live's own preferences if you can reach
   them), check what folder Live's "User Library" / sample browser is
   actually rooted at. Ableton Live's desktop default is normally
   `~/Music/Ableton/User Library/Samples/Imported` — Push 3 standalone's
   embedded install may or may not follow that convention; don't assume.
2. If you have a sample already loaded in Simpler, find Live's own
   process and check what it has open:
   ```
   ssh root@<push-ip>
   pgrep -fl -i live      # find Live's PID
   ls -la /proc/<pid>/cwd
   lsof -p <pid> 2>/dev/null | grep -i -E '\.wav|Sample'
   ```
3. Or just search the filesystem for a folder that already contains
   `.wav`/`.aif` files and looks like a user-facing library, not a
   factory/read-only one:
   ```
   find / -xdev -iname "*.wav" 2>/dev/null | grep -vi factory | head -20
   ```

Once you know the real path, pass it explicitly — nothing here will run
without it (`-sample-dir` flag / `SAMPLE_DIR` env var is required and
fails loudly at startup if unset).

**Record what you find** — once confirmed, it's worth adding a short note
to `docs/environment-and-deploy.md` or `docs/sample-fetch-notes.md` (same
spirit as every other hard-won finding in this repo) so the next person
(or the next time Push's OS gets updated) doesn't have to rediscover it.

## What "load it into a sampler" actually means here

This tool does **not** attempt to remote-control Ableton Live itself
(open a device, hot-swap a sample into an armed Simpler slot, etc.) —
Live is closed-source and GUI-driven, and this repo has no established
automation path into it anywhere (the existing hacks bypass Live's
*engine* entirely via the ALSA loopback; they don't script its UI). Once
the `.wav` lands in a folder Live's browser watches, loading it into
Simpler is just Push's own native hardware browse-and-load — a few
button presses on the device itself, no computer, no extra scripting.
That may need one manual "Rescan" tap on Push's own screen the first
time a new file shows up in a session, depending on whether Live
auto-detects new files in a watched folder or not (also unconfirmed —
note whichever way it goes in `docs/sample-fetch-notes.md`).

## Build & deploy

```bash
cd push-sample-fetch
./build.sh                                   # fetches yt-dlp+ffmpeg into .cache/, builds the Go binary, stages dist/
./deploy.sh <push-ip> <ssh-key> <sample-dir> # scp + relaunch on Push
```

`build.sh` downloads the standalone `yt-dlp_linux` release and a static
`ffmpeg` build (johnvansickle.com) into `.cache/` on first run and reuses
them after — neither is committed to this repo (same reasoning as the
Monomachine ROM not being committed: a third-party binary this repo
doesn't own).

Then from a phone (or anything) on the same wifi as Push:

```
http://<push-ip>:7710/
```

## Config flags / env vars

| Flag              | Env             | Default    | Notes |
|--------------------|-----------------|------------|-------|
| `-sample-dir`       | `SAMPLE_DIR`    | *(none — required)* | see above |
| `-port`             | —               | `7710`     | |
| `-ytdlp`            | —               | `./yt-dlp` | |
| `-ffmpeg`           | —               | `./ffmpeg` | |
| `-cookies`          | `YTDLP_COOKIES` | *(none)*   | Netscape-format `cookies.txt`, only needed for private/login-gated source posts |
| `-max-filesize-mb`  | —               | `200`      | basic guard against grabbing a long video as a "sample" |
| `-fetch-timeout`    | —               | `5m`       | hard cap on the whole download+convert pipeline |

## What it does per request

1. `yt-dlp -x --audio-format wav` — extracts audio directly, no video
   decode/playback involved.
2. `ffmpeg` — optional `-ss`/`-t` trim, optional `loudnorm` loudness
   normalization, and **always** a short (5ms) click-safe fade in/out
   (the "areverse trick" — fades the tail without needing to know the
   clip's duration up front), resampled to 44.1kHz/16-bit stereo.
3. Moves the result into `SAMPLE_DIR` as `<slugified-title>-<unix-ts>.wav`.

Only one fetch runs at a time (Push's CPU is modest and shared with any
other hacks running via `push-hub`) — a second request while one is in
flight gets a plain "already fetching" response rather than queuing.

## Known limitations / not yet verified on real hardware

- `SAMPLE_DIR` — see above, the biggest unknown.
- Whether Live needs a manual "Rescan" for a newly-dropped file to show
  up in its browser, or picks it up automatically.
- `yt-dlp`'s PyInstaller-built Linux binary is dynamically linked against
  glibc (unlike this tool's own Go binary, built `CGO_ENABLED=0` and
  fully static) — `deploy.sh` adds the `/lib64 -> /lib` symlink Push's
  rootfs is missing (see `docs/environment-and-deploy.md`), which should
  cover it, but this hasn't been confirmed against Push's actual glibc
  version.
- Instagram content behind a login (private accounts, some Reels) needs
  a `cookies.txt` passed via `-cookies`/`YTDLP_COOKIES` — public content
  needs nothing extra.
- `/tmp` on Push is tmpfs and wiped on every reboot (see
  `docs/environment-and-deploy.md`) — the default `REMOTE_DIR`
  (`/tmp/sample-fetch-hack`) follows the same convention as the other
  hacks, so a redeploy after a Push reboot is expected, same as them.
