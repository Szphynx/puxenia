# Monomachine port (push-hack-mm / mm-plugin) — architecture and open items

Written while porting Xenia's proven push-hack architecture to Elektron
Monomachine emulation, using `joelanders/gearmulator-md-mm`'s `mdLib`
(a fork of `charlesvestal/gearmulator` — the one Xenia builds against —
that adds full Machinedrum/Monomachine emulation on top of the same
DSP56300 JIT core). Read this before rebuilding or debugging the port;
it records the real API surface this was built against and every
assumption that still needs real-hardware/ROM confirmation, matching this
repo's existing practice (see `docs/xenia-gearmulator-notes.md`'s "Known
open issue" section) of flagging what's unverified instead of presenting
a guess as fact.

**No Push 3 hardware and no Monomachine ROM were available in this
session** — everything below the "Deploying" section is still genuinely
untested against either. What *has* been verified directly, in this
session's own dev environment (not assumed):

- `push-mm` (the Go host) builds clean (`go build`, `go vet`) against the
  real `github.com/federico-pepe/ableton-push-hack/core` module.
- `mm-plugin` builds and links clean end to end — `cmake --build` against
  a full real checkout of `joelanders/gearmulator-md-mm` (`dsp56300` +
  `mc68k` + `baseLib` + `synthLib` + `libresample` + `hardwareLib` +
  `elektron/md/mdLib`, submodules included) produces `libmm-plugin.so`
  exporting exactly one symbol, `move_plugin_init_v2` (confirmed via
  `nm -D`), matching the same symbol-hiding hygiene `xenia-plugin`
  documents for itself.
- Loaded via `dlopen` + `move_plugin_init_v2` (a standalone harness
  mirroring `bridge.c`'s own load sequence) with no ROM present:
  `create_instance` returns a valid instance (not NULL), `get_param(
  "engine_name")` and `get_param("chain_params")` both work immediately
  (59 params — the 58 per-track keys plus `"track"` — returned as valid
  JSON), `get_error` reports a clear `"no valid Monomachine ROM found in
  .../roms (expected an 8MiB .bin matching OS 1.32b)"`, and
  `render_block` fills silence rather than crashing. Repeated with a
  fake random 8MiB `.bin` in `roms/` (to exercise the fingerprint check
  specifically, not just "no file present"): same clean rejection, no
  crash. `destroy_instance` cleaned up without incident in both cases.

What that leaves genuinely unverified: everything that only happens
*after* a real ROM boots successfully — actual audio output, the panel
button/trig-key simulation actually being accepted by the firmware, CC
automation actually reaching the right parameter, and all 5 items in
"Known unverified assumptions" below. Treat those the same way `docs/
environment-and-deploy.md` and `docs/xenia-gearmulator-notes.md` describe
for Xenia's own bring-up: expect to iterate against real hardware and a
real ROM, and update this file with what's found.

## Why Monomachine (not Machinedrum) first

Both machines share `mdLib`'s `md::Device`/`MachineModel` split, so a
Machinedrum port is the same shape (swap `MachineModel::Monomachine` for
`MachineModel::Machinedrum`, use `md::automation::machinedrum`'s page
layout, drop the mute-row's CC number difference). Monomachine was picked
because it's what was asked for.

## The two control surfaces mdLib actually exposes

Unlike Xenia (one MIDI CC chart, transcribed by hand from a manual),
Monomachine has two genuinely separate, both-real control paths in
`mdLib`, and this port uses both for what each is actually for:

1. **MIDI CC "parameter automation"** — `mdLib/mdautomation.h`'s
   `encodeParameterChange`. One MIDI channel per track
   (`baseChannel + track`), a `{page, track, index}` triple maps to a
   fixed CC number per `mmController()` in `mdautomation.cpp`. This is
   Monomachine's own documented external-MIDI-control feature — the same
   category as Xenia's CC chart, except transcribed *by the mdLib
   authors from Elektron's own spec*, not re-transcribed here. Used for
   the 58 per-track params (`mm_plugin.cpp`'s `kParams`).
2. **Front-panel simulation** — `mdLib/mdpanel.h`'s
   `PanelControl`/`panelPacket`/`PanelRowState`, driving
   `Device::sendPanelEvent` exactly as a physical button press would.
   This is the *only* way to reach the 16 trig keys, 6 track-select
   buttons, and transport (Play/Stop/Record) — none of those have a MIDI
   equivalent on real hardware, so there's nothing to transcribe; this
   port drives the emulated panel UART directly.

## Push 3 control mapping

Everything below is live (not aspirational) in the code as written — see
`push-hack-mm/src/main.go`, `params.go`, `audiosession.go`, `leds.go`,
`display.go` for the implementation.

### Knobs (the 8 encoders above the screen)

Each of the 8 top-screen pages (below) puts up to 8 real Monomachine
parameters on the 8 encoders, always acting on **whichever track is
currently selected** (see "Track select" below) — turning an encoder
sends that param's CC automation message for that track. The one
exception is the SEQ page, where encoder 1 is repurposed as **BASE
CHANNEL** (0-15, the MIDI channel track 1 uses — track *N* always uses
`BASE CHANNEL + N`) since there was no spare SETTINGS column left for it.

### Pages (top-screen buttons 1-8, 2 banks via D-Pad Left/Right)

| Bank 0 | Bank 1 |
|---|---|
| SYNTH (Synthesis A-H) | LFO 3 |
| AMP (Attack/Hold/Decay/Release/Distortion/Volume/Pan/Portamento) | LEVEL (Level, Mute) |
| FILTER (Base/Width/HP Q/LP Q/Attack/Decay/Base Offset/Width Offset) | *(spare)* |
| EFFECTS (EQ Freq/Gain, Sample Rate Reduction, Delay Time/Send/Feedback/Base/Width) | *(spare)* |
| LFO 1 | *(spare)* |
| LFO 2 | *(spare)* |
| **SEQ** (pinned, both banks) | **SEQ** |
| **SETTINGS** (pinned, both banks) | **SETTINGS** |

This mirrors Xenia's own "11 groups don't fit 8 buttons → 2 banks, pin
the special pages in both" pattern (`push-hack-xenia/src/params.go`'s
`bankPageNames` doc) — here 7 synth pages + LEVEL don't fit 8 either once
SEQ/SETTINGS are added.

### Menus

- **SEQ page bottom buttons**: `PAGE` (flip steps 1-8 ↔ 9-16), `PLAY`,
  `STOP`, `REC` (transport — taps the real Play/Stop/Record panel
  buttons).
- **SETTINGS page** (identical to Xenia's): 4 columns — MIDI IN, AUDIO
  OUTPUT, AUDIO CHANNEL, MIDI CHANNEL — each its own encoder (1/3/5/7) +
  commit button (1/3/5/7).
- **Web UI** (`ui/index.html`): the same pages as tabs, plus a dedicated
  Seq tab with the step grid, transport buttons, track picker, and a
  decoded LCD framebuffer canvas.

### Pads (the 8×8 grid) — dual-mode

- **Normal (any page except SEQ)**: a pad press plays the **currently
  selected track** live — exactly like every pad always played a note in
  Xenia. `mm_plugin.cpp`'s `on_midi` remaps the channel internally
  (`baseChannel + currentTrack`), so `main.go` never rewrites the channel
  itself, same division of labor Xenia already had.
- **SEQ page (on-screen UI open, SEQ tab showing)**: pads become a
  **6-track × 16-step grid sequencer**, directly answering "access each
  voice with the pads": rows 0-5 (bottom-up) = tracks 1-6's current
  8-step page (`PAGE` button flips 1-8 ↔ 9-16), row 7 = per-track mute
  toggle (columns 0-5), row 6 reserved/dark. A step pad press calls
  `mm_plugin.cpp`'s `toggle_step`, which selects that row's track (taps
  the real Track button) then taps that step's real Trigger key —
  **this drives mdLib's own actual Monomachine firmware sequencer**, not
  a reimplementation, so pattern length, swing, per-step behavior etc.
  are all the real machine's.
- **Track select**: D-Pad Up/Down, works on every page — also implicitly
  changed by tapping any step in SEQ mode (matches physically pressing a
  track's own trig key on real hardware, which also selects that track).

### Pad LEDs

Real Note On messages, velocity = Push 3 palette index, sent directly
over this hack's own ALSA seq port (not a push-manager display-API call —
see `leds.go`'s doc; Push pad RGB genuinely is just a Note On with
velocity-as-color). The currently-selected track's row is driven by
`mdLib`'s own live `FrontPanel::getMonomachineStepLedColor` readback
(so it also shows playhead flashing during playback); every other row is
this host's own shadow (`seq.go`), since real hardware has no way to read
back a non-selected track's trig LEDs either.

## Known unverified assumptions (verify against real hardware/manual)

1. **`toggle_step` assumes no separate "grid record" mode is needed** to
   toggle a step on/off by tapping its trig key — i.e. that Monomachine's
   trig keys work as a direct step-programming grid outside of live
   RECORD (which is assumed to be for recording played notes/knob moves
   in real time, a separate concept). `mdLib/mdfrontpanel.h`'s
   `ModeLed::Record` is commented `// grid-edit`, which casts some doubt
   on this — it may mean grid step-editing genuinely is a distinct mode
   that needs arming first. If steps don't toggle on real hardware,
   the fix is one line in `mm_plugin.cpp`'s `toggleStep`: hold
   `PanelControl::Record` for the duration of the tap.
2. **No "is playing" / "is recording" LED was found** in `FrontPanel`'s
   `StatusLed`/`ModeLed` enums with unambiguous transport semantics —
   `StatusLed::Pattern` reads as "Pattern mode active" (vs Song mode),
   not playback state. `push-mm`'s Play/Stop/Record buttons therefore
   drive a purely optimistic on-screen/web shadow (`seq.go`'s
   `playing`/`recording` fields), not a hardware-confirmed readback.
   `mm_plugin.cpp`'s `panel_state` does expose the raw Mode LED bank byte
   (`modeLed`) for whoever picks this up to decode properly against real
   hardware behavior.
3. ~~CMake dependency graph for `mm-plugin` unbuilt~~ — **resolved**: this
   was assembled by reading `gearmulator-md-mm`'s own top-level `source/
   CMakeLists.txt` and each library's `CMakeLists.txt` directly (not
   copy-pasted from `xenia-plugin/CMakeLists.txt`, whose
   `charlesvestal/gearmulator` target uses a different layout —
   `framework/<lib>` vs this fork's flat `source/<lib>`), and now builds
   clean end to end (see the verified-build list above). One real gap it
   found: `synthLib` unconditionally links a `resample` target
   (`target_link_libraries(synthLib PUBLIC resample dsp56kBase baseLib)`
   in its own CMakeLists.txt) even though `mm-plugin` itself never calls
   into `libresample` — omitting that `add_subdirectory` configures fine
   but fails at final link with `cannot find -lresample`; `mm-plugin/
   CMakeLists.txt` now includes it with a comment explaining why.
4. **`toggle_mute`/per-track mute** sends Monomachine's real Mute CC (page
   8, index 0 in `mdautomation.cpp`'s `mmController`) for an explicit
   track without switching to it first (unlike `toggle_step`, a CC
   carries its own destination channel already). Not yet confirmed this
   CC actually mutes the track the way the SEQ page's mute row implies on
   real hardware — plausible from the automation table's own naming, not
   independently confirmed.
5. **ROM discovery** (`synthLib::RomLoader::addSearchPath` +
   `md::RomLoader::findROM(MachineModel::Monomachine)`) expects an 8MiB
   `.bin` in `module/roms/` matching OS 1.32b's fingerprint
   (`mdtypes.h`'s `g_mmOs132bFingerprint`). Verified in this session that
   both "no file present" and "an 8MiB file present but wrong
   fingerprint" are rejected cleanly (see the verified-build list above)
   — genuinely unverified is only the success path, an actual real ROM
   being found and booted.

## Deploying

See `push-hack-mm/build.sh` and `push-hack-mm/deploy.sh` — same shape as
`docs/environment-and-deploy.md`'s documented Xenia deploy loop, just
pointed at this hack's own paths and `GEARMULATOR_MD_MM_SOURCE`. A legally
owned Monomachine OS 1.32b ROM must be placed at
`push-hack-mm/module/roms/<name>.bin` before deploying — not included,
same posture this fork's own README states for MD/MM firmware in general.
