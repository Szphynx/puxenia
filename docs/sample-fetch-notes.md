# push-sample-fetch notes

`push-sample-fetch/` is a new, separate tool (not a `push-hub`-registered
hack — no MIDI, no screen, no ALSA) that runs its own tiny web service
directly on Push 3's embedded Linux: paste a video URL from a phone on
the same wifi, it pulls the audio out with `yt-dlp`, cleans it up with
`ffmpeg`, and drops a `.wav` into a folder Ableton Live's own User
Library browser watches. Full design/usage is in
`push-sample-fetch/README.md` — this file is for hardware findings once
someone actually runs it against a real Push, same pattern as this
file's siblings (`audio-pipeline-debugging.md`,
`environment-and-deploy.md`).

## Open questions — nothing below is confirmed yet

- **Live's actual User Library / sample folder path on Push 3
  standalone.** `push-sample-fetch` refuses to start without an explicit
  `SAMPLE_DIR` for exactly this reason — see the README's "finding your
  Push's sample folder" section for how to locate it. Record the actual
  path here once found, so it doesn't need rediscovering:
  - *(not yet filled in)*
- **Does Live auto-detect a new file dropped into that folder, or does
  it need a manual "Rescan" tap in its own browser UI?** Affects whether
  "paste URL → fetch → load in Simpler" is fully hands-off after the
  fetch, or needs one extra tap on Push's own screen each time.
- **yt-dlp's prebuilt Linux binary on Push's actual rootfs** — it's a
  PyInstaller build, dynamically linked against glibc (unlike this
  project's Go binaries, which are `CGO_ENABLED=0` and fully static).
  `deploy.sh` adds the `/lib64 -> /lib` symlink this repo's other deploys
  already need (see `environment-and-deploy.md`), which should be
  sufficient by the same reasoning as `push-xenia`'s own binary working —
  but this specific binary hasn't actually been run on Push yet.
- **Instagram-specific behavior**: public Reels/posts should need
  nothing extra; private/login-gated content needs a `cookies.txt`
  (`-cookies`/`YTDLP_COOKIES`) exported from a logged-in browser session.
  Not yet tested against a real private post.

## Why "play the video and record it" was not the approach taken

The user's original framing was "play it and record it" to get audio
onto Push. `yt-dlp -x` extracts the audio track directly from the
downloaded video file instead — strictly better (lossless, no ambient
noise, no playback/capture device needed, no level-matching), and the
video file already contains the audio track as data, so there's no
reason to round-trip it through a speaker and a microphone. If a future
need ever requires literally capturing live audio playing on Push
through `push-audio-loopback` (e.g. sampling audio actually being
rendered by one of the other hacks, not from a downloaded file), that
would be a genuinely different tool — reusing `ensure-audio-loopback.sh`
and the loopback card's other subdevices — not a fit for
`push-sample-fetch`, which is purely a "fetch from a URL" tool.
