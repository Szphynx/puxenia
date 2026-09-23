# Audio pipeline debugging notes (ROM path, garbling, stutter)

Everything below was learned during one long live-debugging session getting
audio actually flowing from Push 3 → this plugin → ALSA loopback → Ableton
Live. Read this before re-investigating "no audio," "garbled/timestretched
audio," or "clicking/stuttering audio" — all three symptom classes were hit
and root-caused here; don't re-derive them from scratch.

## 1. "MIDI comes in, plugin loads, but there's total silence" — check
## boot failure first, not routing

**Root cause found here**: `xenia_create_instance` does `chdir(module_dir +
"/roms")` before loading the ROM images. `deploy.sh` was copying the ROM
files flat into `<module_dir>/` instead of `<module_dir>/roms/`. The ROM
load then failed, which set `inst->bootFailed = true` — but **nothing
checked for that**. `bridge_plugin_load` only checked `instance != NULL`
(true even for a boot-failed instance), so the Go host logged "plugin
loaded and instance created" and ran completely normally. `xenia_render_block`
just `memset`s its output to zero and returns when `bootFailed` is set —
producing perfect, permanent digital silence with **no error, no crash, no
log line, nothing** to distinguish it from a correctly-routed but silent
signal chain.

This produced hours of chasing the wrong thing: MIDI routing, ALSA device
selection, channel numbers, Live's input monitoring — all correct the
whole time. The bug was 100% on the plugin-load side, invisible from every
angle except "is bootFailed actually false."

**Fixes applied (both needed — keep both)**:
- `deploy.sh`: ROMs now copied to `$REMOTE_DIR/module/roms/` (not
  `module/` directly), with `mkdir -p` for that path.
- `bridge.c`'s `bridge_plugin_load`: after `create_instance` returns
  non-NULL, now also calls `api->get_error` and fails loudly (returns NULL,
  calls `set_error`) if the plugin reports an internal error. **This is the
  general-purpose fix** — it means any *future* silent-boot-failure mode
  will now surface as a load error instead of silent zero output. Don't
  remove this check to "simplify" the load path.

**Directive**: if audio is completely silent (not garbled, not clipped —
literally nothing, ever) despite MIDI visibly arriving and routing/device
settings looking correct, the first thing to check is whether the plugin
instance actually booted, not the audio routing. A silent instance and a
correctly-routed-but-unplayed instance look identical from every log
except this one.

## 2. "Audio exists now but sounds garbled/timestretched/like a bad codec"
## — check Ableton Live's own audio buffer size FIRST, before touching
## this codebase's DSP/resample code at all

This was the single biggest time sink of the session, and the eventual fix
was **entirely outside this repo**: raising **Ableton Live's own Audio
Engine buffer size to 512 samples @ 48kHz**. Confirmed independently by a
parallel, completely unrelated debugging session (a different hardware
synth, Elektron Monomachine) hitting the **identical** "metallic bleeping,
bad-codec" symptom and being fixed the same way — proving the symptom is a
property of Push 3's audio hardware / Live's engine interaction under a too
-small buffer, not anything specific to this plugin's DSP.

Several *real* bugs were found and fixed in this codebase while chasing
this symptom, and all of them were legitimate, necessary fixes — but **none
of them was the actual cause of the garbling**, and none of them alone
fixed it:
- Resampler was silently dropping unconsumed input samples every block
  (ignoring `inBufferUsed` from `resample_process`) — fixed via a
  `PerChannelResampler.leftover` carry-forward field, matching
  gearmulator's own reference pattern. Verified clean via a standalone
  offline test tool (`resample_probe.cpp`, links directly against
  gearmulator's static libs, bypasses ALSA/Go/Live entirely) plus a Python
  sample-difference analysis — found no jumps once fixed.
- Stale placeholder sample rate: the plugin was initialized at a
  placeholder 44100 Hz at load time, then never corrected once the real
  negotiated ALSA rate (48000) was known — fixed via a reserved
  `"_host_sample_rate"` key sent through `xenia_set_param` right after the
  PCM device actually opens.
- MPE pitch-bend forwarding was adding jitter/complexity for no audible
  benefit — disabled per explicit user call ("should we not just turn off
  the MPE and focus on the midi note? ... clearly the 4am version worked
  well and did sound amazing"). Simpler is more reliable here; don't
  re-add MPE pitch-bend forwarding without a concrete reason.

**Directive**: if audio sounds timestretched/garbled/bit-crushed/like a bad
phone codec, check Live's own Audio Engine buffer size (Settings → Audio)
**before** spending hours in this plugin's resample/DSP code. 512
samples @ 48kHz is the confirmed-working baseline on Push 3 hardware for
this class of symptom. Smaller buffers (128/256) are exactly the ones that
reproduce it. This is a hardware/host-engine interaction, not a bug in this
plugin's code, even though it's tempting to assume otherwise once you're
staring at resample code and it does have a real (unrelated) bug.

## 3. Clicks/hiccups correlated with terminal log lines — never let the
## render thread touch stdout

**Symptom, reported verbatim**: "i believe evertime i see a line appear on
the terminal the audio stutters slightly." This is real and was a
plumbing bug, not perception: any `log.Printf`/`fprintf` call made
**synchronously from the audio render path** blocks that goroutine/thread
on stdout I/O (buffering, terminal flush, SSH pty latency when watching
logs remotely), which is real-time-unsafe by construction — a real-time
audio callback must never do blocking I/O.

**Fixes applied**:
- `xenia_plugin.cpp`: removed the per-parameter-change debug `fprintf`
  calls entirely from `xenia_set_param` (they fired on *every* knob/CC
  change, i.e. constantly during normal play).
- `audiosession.go`: added an async, non-blocking logger (`alogf`) — a
  buffered channel (cap 64) drained by a dedicated background goroutine,
  with a non-blocking `select`/`default` send so a full channel drops the
  line rather than blocking the caller. The hot-loop `log.Printf` calls
  (SLOW BLOCK warning, periodic progress report, PCM xrun retry log) were
  switched to `alogf`. One-time calls that don't recur during playback
  (session-open, SCHED_FIFO warning, session-stop) were deliberately left
  as plain `log.Printf` — no need to complicate those.

**Directive**: any logging added inside the audio render loop, or inside
any function called from it (including `xenia_set_param`, called
synchronously on every incoming CC/param change), must go through the
async logger (`alogf`) or be removed, never a direct blocking
`fprintf`/`log.Printf`. This applies to *new* code too, not just what's
already fixed — a future contributor adding a debug print inside the hot
path will reproduce this exact symptom.

## 4. LCD logging (`MCLOG`) flooding and blocking the render thread

`xtPic.cpp`'s LCD console logging (`mc68k::logToConsole`) was a hardcoded
`fputs(..., stderr)` with **no override hook**, unlike `dsp56kBase`'s own
logger (which already had `Logging::setLogFunc`). This meant the emulated
LCD's constant internal chatter was unconditionally flooding stderr and
blocking the render thread, same class of bug as #3 above but with no way
to just swap in a callback.

**Fix — "static-library override trick"**: define a function with the
exact same signature in a directly-linked object file (this plugin's own
`.cpp`), so the linker resolves the symbol from there and never pulls in
`libwLib.a`'s own definition of it. This is the general pattern to reach
for whenever a static library defines a hardcoded logging/callback
function without an override hook: **don't patch the vendored library
source** (gearmulator is a vendored dependency, not this repo's own code)
— link-order-override it from this repo instead.

This capture point later became useful, not just a fix: the LCD's text
content turned out to be the **only source of real patch names and bank
letters** on this device (no MIDI name-readback exists), so the same
capture buffer (`g_lcdLine1`/`g_lcdLine2`) that stopped the log flood is
now also parsed for the PRESETS page's live patch-name display. Don't
remove the capture in a future cleanup pass just because it looks like
leftover debug plumbing — check `xenia_get_param`'s `lcd_patch_id`/
`lcd_patch_name` keys before deleting it.

## 5. ALSA loopback device numbering — device 0 vs device 1 cross-wiring

The loopback card exposes two devices; Live's input side and this plugin's
output side must be on **opposite** devices of the same card (e.g. plugin
writes to device 1, Live reads from device 0's paired side) — pointing
both sides at the same device number silently produces no audio path at
all, same "looks correctly configured, isn't" trap as the ROM-path bug
above. Also watch for the **real hardware audio card** (e.g. Push's own
onboard card) being accidentally selectable in the AUDIO OUTPUT picker —
filter that picker to the loopback card specifically, don't rely on the
user picking correctly from an unfiltered device list.

## 6. Repeated kernel-module reload cycles can orphan Live's ALSA handle

Reloading `snd-aloop` repeatedly (e.g. while iterating on `deploy.sh`)
without a clean unload first was observed to produce a hardware I/O error
(`rc=-5`, EIO) that persisted until a **full Push reboot** — not fixable
in software once it happens. If `insmod`/`modprobe` on `snd-aloop` is
needed mid-session, check `/proc/asound/cards` first and avoid blindly
reloading a module that Live already has a device open against; don't try
to force an `id=` param on `insmod` that would require an `rmmod` first,
since that `rmmod` fails outright while Live holds the paired device open.

## 7. Git branch/worktree hygiene when two unrelated projects share one
## checkout directory

This repo's `main` branch (this Xenia/Push work) and a separate, unrelated
project (an Elektron Monomachine hack, branch `sph/busy-goldberg-ajmuyj`)
were both being developed against the **same** `~/puxenia` checkout
directory on the user's machine at different points in the session. Any
time the checkout had the Monomachine branch checked out, files that only
exist on `main` (like `deploy.sh`) appeared to have vanished — not a bug,
just `git checkout` doing exactly what it's supposed to.

**Fix**: `git worktree add ~/puxenia-mm sph/busy-goldberg-ajmuyj` gives the
unrelated branch its own separate working directory, so switching between
the two projects never again requires a `git checkout` in the shared
directory. Note the ordering constraint: `git worktree add` fails with
`fatal: '<branch>' is already checked out at '<path>'` if that branch is
still checked out in the original directory — checkout `main` in the
original directory *first*, then add the worktree for the other branch.

**Directive**: if `deploy.sh` (or any other file known to exist on `main`)
"disappears" after working on it, check `git branch --show-current` before
assuming file corruption or a bad `git pull` — it's very likely just a
different branch checked out in the same directory from unrelated work.

## 8. "Hangs and drags on sounds" after turning a knob — SysEx pacing, not
## a DSP/resampler bug

Reported after `params.go`'s params were switched from plain 3-byte CC
messages to `xenia_plugin.cpp`'s SysEx Single Parameter Change (10 bytes,
see `docs/xenia-gearmulator-notes.md`'s "The real answer" section — most
Xenia params need this, only 7 CCs are hardwired). `gearmulator`'s own
`hardwareLib/sciMidi.cpp` (`SciMidi::process`) models a real MIDI cable's
byte rate for outgoing SysEx: each queued message serializes for
`msg.size() * m_samplerate * 0.1 / 500` samples before the next queued one
can even start draining into the emulated UART — realistic, but it means a
fast continuous knob turn or a dragged web-UI slider (both can fire many
`set_param` calls per second) queues SysEx messages faster than the
emulated device drains them, and the audible parameter change visibly lags
behind the physical motion while the backlog empties. Plain-CC params
(the 5 hardwired ones, and note/PC messages) aren't paced this way and
never showed this symptom.

**Fix**: `audiosession.go`'s debounce mechanism (previously only used for
"program", 200ms, to avoid flooding the device's patch-load state
machine) is now generic — every device-facing param commit goes through a
30ms per-key coalescing timer (`scheduleParamCommit`/`paramDebounceDelay`)
before it's actually written to the plugin. The on-screen/web value still
updates immediately (unaffected — that's `applyEncoder`'s own
`nudgeSlotLocked`, screen-only); only the SysEx write to the device is
coalesced, so a fast flick sends one settled commit instead of a burst.
**Not yet re-tested on real Push hardware as of this writing** — reasoned
from `sciMidi.cpp`'s actual pacing constants, not from a live repro, since
this environment has no ROM/hardware to reproduce the original symptom
against. If "hangs and drags" persists after this, the debounce delay
(30ms) may need to go up, or another cause entirely is in play — don't
assume this was the only possible source without re-testing.

## 9. "Audio not ready" on BOTH puXenia and puMMa, together, for no
## apparent reason — an ALSA hw device conflict BETWEEN the two hacks,
## not a Live setting

Reported after a full `deploy-all.sh` (all three hacks) for the first
time: puXenia showed "not ready" despite nothing about its own Live
routing having changed, and puMMa showed the same. Root cause: both
hacks' `config.go` defaulted to the **identical** PCM device,
`hw:Audio,1,0`. An ALSA hw device is exclusive-access — two separate
processes opening the same one for playback at once means the second
`bridge_pcm_open` fails outright (EBUSY), which `watchHWParams` then
reports as `msgWaitingForLive` ("go check your Live routing") — a
genuinely misleading message, since the real conflict is with the OTHER
hack, not anything wrong in Live. This never showed up before because
only one hack ever ran at a time before push-hub made running both
together the normal case.

**Fix**: `push-hack-mm`'s default `PCMDevice` moved to `hw:Audio,1,1`
(subdevice 1) — `push-hack-xenia` keeps `,1,0` unchanged. snd-aloop's own
default `pcm_substreams` is 8 per device (this repo's `deploy.sh` insmods
it with no override), so subdevice 1 is a real, independent PCM stream,
not a guess. **This needs a matching change in Live**, not just the
code: puMMa now needs its OWN separate audio track routed to the
loopback card's *second* input (subdevice 1), distinct from whatever
track puXenia already uses — `msgWaitingForLive`'s on-screen instructions
now say so, but the Live-side track itself still has to be added by
hand, same one-time setup as puXenia's own track originally was.

**Directive**: if two hacks are ever meant to run simultaneously and both
write audio, check they don't share a PCM device string before assuming
either one's own audio pipeline is broken — an ALSA "not ready"/EBUSY
symptom from a device conflict between two of THIS project's OWN
processes looks identical to a Live-routing problem from the affected
hack's own point of view.

**Follow-up, still not fully hardware-confirmed**: the subdevice-1 fix
above didn't fully resolve it on its own — Live's own routing UI for the
loopback card only offered individual mono channels to pick from (e.g.
"channel 3" alone), not a paired stereo input the way puXenia's own
existing track has, and there was no way to change which device/
subdevice a hack targets without editing source and rebuilding. Two
follow-up fixes, both still needing a real hardware pass to confirm they
actually resolve it:
- `watchHWParams`'s "not ready" message (both hacks) now shows the
  actual `bridge_pcm_open` error and device string when that's the real
  failure, instead of always showing the same generic "go check Live"
  text regardless of cause — this is what should have surfaced the "Text
  file busy"-class of real error immediately instead of needing this
  whole investigation.
- Both hacks' own on-screen AUDIO OUTPUT picker (`iopage.go`) now lists
  every subdevice of the loopback card individually, not just subdevice
  0 (`alsapcm.PlaybackDevice.HWDevice()` always hardcodes subdevice 0 —
  its own doc says so — so the picker silently could never have offered
  anything else before this). Nothing about which device a hack uses is
  hardcoded anymore; it's fully user-selectable from SETTINGS.

## General debugging directive for this project

Given how many of the above turned out to be "looks completely correct
from every visible signal, but isn't" (bootFailed with no propagated
error; identical device numbers on both sides of the loopback; a garbling
symptom that's actually a Live setting, not this plugin's DSP), the
standing lesson is: **when a symptom is "everything looks right but it's
still broken," suspect a state that's silently wrong rather than a
routing/config value that's visibly wrong.** Add an explicit error check
or an observable log point at each such boundary as it's found (as
`bridge_plugin_load`'s `get_error` check now does) rather than only fixing
the one instance — the next silent-failure mode won't announce itself any
more than this one did.
