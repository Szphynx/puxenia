# Monomachine hardware-parity TODO

Findings from comparing the port (`mm-plugin/`, `push-hack-mm/`, and the
`docs/mockups/push-mm-interface.html` mockup) against real Monomachine
hardware behavior, researched 2026-09-18. Sources noted per item — most of
the official Elektron/Sound on Sound/manual sites were blocked by this
session's egress proxy, so several items are search-snippet-sourced only,
not full-manual-verified. Treat unverified items the same way
`monomachine-port-notes.md` already treats its own open assumptions: don't
re-guess them, go straight to hardware/manual when available.

## Full sequencing audit (requested before further real-hardware testing)

Went through every sequencing-related path end to end (`seq.go`,
`doTransport`, `mm_plugin.cpp`'s panel/transport handling, `main.go`'s pad
routing, `leds.go`, `webserver.go`'s `/api/seq/*` + `/api/transport` —
the web UI routes through the exact same `ctlToggleStep`/`ctlMuteToggle`/
`ctlTransport` events as the pads, so it shares fixes and bugs with them,
not a separate code path). Findings:

- [x] **Play/Stop/Record buttons (`doTransport`) DO register**, verified
  empirically — unlike the trigger-key tap bug above, a zero-elapsed-time
  press+release of the Play panel button reliably started something real:
  `panel_state` changed continuously on its own for 10+ seconds afterward
  (a clean control run with no Play press showed zero spontaneous change
  over the same window). Not every panel control needed the deferred-
  release fix — Play/Stop appear to register synchronously just fine.
- [ ] **`seqState.ToggleStep`'s plain boolean model is provably wrong.**
  Confirmed via the same real-ROM test that proved grid editing now
  reaches the device (see above): tapping the same step twice in a row
  left its panel-readback color unchanged both times — it did NOT toggle
  back to 0. This host's own shadow (`seq.go`) still flips a plain bool
  on every tap, so after 2 taps on the same step this shadow reads "off"
  while the real device almost certainly still reads whatever
  cycled-through state it landed on. Needs live-hardware-informed
  testing (repeatedly tap the same pad on real Push, see what the real
  LED does) before changing `seqState`'s model — don't guess a fix.
- [ ] **`toggle_mute` (see "needs verification" below) is the other open
  item from this pass** — CC reaches the device, real muting effect not
  confirmed.
- [x] **Web UI's `/api/seq/step`, `/api/seq/mute`, `/api/transport`
  handlers are just thin wrappers around the same control events the
  pads use** (`webserver.go`'s `handleSeqStep`/`handleSeqMute`/
  `handleTransport`) — no separate bug surface there, they inherit
  whatever pad-path fixes/bugs apply.

## Ableton Live transport sync (new feature, investigated, NOT working yet)

Requested: puMMa's sequencer should start/stop with Ableton's own global
transport, and reset when Live stops.

- Real Monomachine firmware very likely supports MIDI realtime transport
  sync (Clock/Start/Continue/Stop, 0xF8-0xFF) as a genuine hardware
  feature — confirmed this is deliberately modeled, not guessed:
  `mdhardware.cpp`'s MIDI-in byte pump computes the wire byte count from
  the status byte itself (1 byte for status ≥0xF0, correctly ignoring
  any stray data bytes), and a separate `pumpRealtime()` path lets these
  bytes cross ahead of other queued MIDI mid-stream — exactly how real
  hardware treats realtime bytes. `mm_plugin.cpp`'s `mm_on_midi` was
  the only thing blocking them (an explicit channel-voice-only filter);
  now forwards status ≥0xF8 straight through.
- **Tested against the real ROM, negative result**: sending a raw MIDI
  Start (0xFA) did NOT start the sequencer — `panel_state` stayed static
  for 5+ seconds afterward, unlike a genuine Play button press. Most
  likely explanation: real Monomachine's Global settings almost
  certainly default its Clock Source to Internal (standard for hardware
  sequencers of this era), and needs that explicitly switched to
  External/Auto before it'll honor incoming transport bytes at all —
  not yet found how (SysEx global-parameter write, or a panel/menu
  navigation sequence — needs real research, not a guess).
- **Separately unsolved**: even once Monomachine itself can be made to
  listen, this host has no confirmed way to detect Ableton's OWN
  transport state to forward it in the first place. `pmclient`'s entire
  API surface is `SetMode`/`PushImage`/`DisplayStatus`/`SetMidiFilter`/
  `Tempo` — no transport/play-state endpoint. push-manager may expose
  more than pmclient wraps (unconfirmed, no access to its own source
  from this session), or Live's own MIDI Clock/transport output could be
  routed to puMMa's existing MIDI-in port instead (requires the user's
  own Live-side Sync preferences, not something this bridge controls).
  Two real unknowns, not one — scope this properly (its own doc, like
  `push-hub-proposal.md`) before implementing rather than half-building
  a feature on unconfirmed assumptions in both directions.

## Confirmed real, not yet matched in code

- [x] **Grid Recording is a real, separate mode gated by Record — gate
  added AND now verified reaching the device.** Per the Elektron
  quick-start guide: "You enter Grid mode by hitting the Record button,
  whereupon its red LED will light." `toggleStep()` holds
  `PanelControl::Record` for the duration of the trig tap. First attempt
  (press+tap+release as one synchronous call, zero elapsed device-time)
  was proven NOT sufficient by a real-ROM dlopen test: `panel_state`'s
  per-step LED read unchanged even though the LCD content changed.
  **Fixed properly**: panel-button releases are now deferred
  (`MmInstance::pendingPanelReleases`, drained at the top of every
  `mm_render_block`) so the emulated firmware gets `kPanelHoldFrames`
  (2048 samples, matching `mdLibTest`'s own `tap()` helper exactly) of
  real elapsed device-time between a press and its release, without
  stealing samples from the host's own real-time audio output. Re-ran
  the same real-ROM test: `toggle_step` now DOES flip the target step's
  panel-readback LED (0 → 2/red). SEQ page pad presses now reach the
  real device, not just this bridge's own shadow.
- [ ] **Two new open questions from that same verification pass, real
  behavior not yet fully understood:**
  1. Tapping the SAME step twice in a row did not toggle it back off —
     it read the same non-zero value both times. `LedColor` has 4
     values (0=off/1=green/2=red/3=yellow); this suggests a trig key in
     grid mode may cycle through note/trigless states rather than being
     a true binary on/off, which would make `toggleStep`'s name (and
     the Go host's `seqState.ToggleStep`, a plain bool flip) a wrong
     model of the real behavior. Needs live-hardware-informed testing
     (tap the same pad repeatedly on real Push and see what actually
     happens) before touching the Go-side toggle logic — don't guess at
     a fix without that.
  2. Toggling step 3 also changed step 0's reported LED color, with no
     action taken on step 0. Not yet root-caused — could be a genuine
     `triggerControl`/`panelPacket` indexing issue, or a real hardware
     behavior (e.g. a downbeat/reference marker) misread as a bug.
     Flag, don't fix blind.
- [ ] **`FUNCTION` + `LEFT`/`RIGHT` in grid mode rotates the current
  track's trigs and locks by one step.** Not exposed anywhere in
  `push-hack-mm` today. Real feature, not required for parity, but easy
  to add once grid mode itself is fixed: SEQ page, Shift+D-Pad
  Left/Right (D-Pad alone is already track-select) or a bottom-strip
  button.
- [ ] **Parameter locks** ("the secret weapon of the Monomachine" — hold
  a trig key, turn an encoder A–H to lock that param at that step) are a
  real, load-bearing feature with zero support in this port. Worth a
  dedicated design pass, not a quick add — needs panel-encoder-press
  simulation (`mdpanel.h`'s `panelEncoderPressPacket`, already read but
  unused) combined with a held trig key.

## Needs verification, not yet confirmed either way

- [ ] **`toggle_mute`'s CC likely doesn't do real per-track muting.**
  Verified against the real ROM: `toggle_mute` (Mute CC, page 8 index 0,
  matches `mdautomation.cpp`'s own table) does reach the device — it's
  not silently dropped — but muting a track while it had a live-triggered
  note sounding only reduced output by ~8%, nowhere near real silence.
  Reported as "mutes don't work at all" after real hardware testing.
  Two live possibilities, not yet disambiguated: (a) the CC mapping/effect
  itself is wrong, or (b) real Monomachine Mute may only affect a track's
  *sequenced* playback, not live/manually-triggered notes (common Elektron
  semantics) — in which case this port's behavior would be correct and
  the test (which used a live-triggered note, not Play) simply wasn't
  testing the right thing. Needs testing on real hardware specifically
  *while Play is running* before touching the code.
- [ ] **No playhead / step-position indicator exists anywhere in this
  port**, on-screen or on the pads — reported as a "grey bar that should
  move right during playback" after real hardware testing, but there is
  no such element in `display.go` or `leds.go` at all. What's actually
  being seen: the SEQ page's pad-grid LED sync (`leds.go`'s
  `stepLedColor`) draws the *currently selected track's* row using a
  live device readback (`padStepOff`, dim gray, for an unprogrammed
  step) while every other track's row uses this host's own static
  shadow — and `toggleStep` auto-selects whichever track's row you tap
  a step on, so that dim-gray row visually jumps between rows as you
  interact with different tracks. Easy to mistake for a moving
  indicator; it isn't one. A real playhead needs actual step-position
  telemetry from the device, which this bridge doesn't have a confirmed
  source for yet (see the existing "no confirmed is-playing/is-recording
  LED" item above) — real feature, not a quick fix.
- [ ] **TRIG LED color meanings beyond green/yellow.** Yellow =
  trigless/NOTE-OFF marker, green = normal note trig — matches
  `FrontPanel::LedColor`'s enum already. What **red** means on a step
  LED specifically (vs. Record's own red status LED, a different thing)
  is still not confirmed from any source reached this session. Don't
  trust `padStepOnRed`/`FrontPanel::LedColor::Red`'s current "just
  another step color" treatment in `leds.go` until this is checked
  against the real manual's TRIG LED section.
- [ ] **LCD color/backlight.** `display.go`'s `mmChassis`/`mmGreen`/
  `mmAmber` theme and the mockup's green-on-black LCD are **not**
  sourced from anything confirmed this session — moderate-confidence
  guess only, era-correct Elektron gear more likely reads grey/blue-ish
  with dark text, not green/amber. Get an actual photo or the manual's
  own screenshots before trusting this palette as "authentic Monomachine
  look" in any user-facing copy.
- [ ] **9 physical encoders, not 8.** Reverb listing + manual snippet:
  DATA ENTRY A–H (8) plus LEVEL and SOUND SELECTION as separate
  dedicated encoders (parameter-lock doc explicitly excludes LEVEL/SOUND
  SELECTION from the A–H lock behavior, implying they're physically
  distinct controls). Push only has 8 general encoders + 1 dedicated
  Volume knob, so this doesn't change the port's own design — see the
  Push-space item below — just don't describe the port's 8-knob pages as
  "the same as the real panel" without qualifying it.

## Push-space usage — better fits than what's shipped today

- [ ] **Use Push's real hardware Play/Record buttons (`push3.CCPlay`,
  `push3.CCRecord`) for SEQ transport instead of soft bottom-strip
  labels.** Real Monomachine gates trig-editing on Record's own physical
  LED — Push has a dedicated Record button sitting unused today
  (`litCCs` lists it, nothing binds it). Wiring it to the same
  Record-gated grid-mode fix above is more correct AND frees 2 of the 4
  SEQ bottom-strip slots (`PAGE`/`PLAY`/`STOP`/`REC` → just `PAGE` +
  maybe one more) for something that doesn't have a hardware button
  equivalent, e.g. COPY TRACK, CLEAR STEP PAGE, or a pattern-number
  readout.
- [ ] **Use `push3.CCVolume` (the dedicated hardware Volume knob) as the
  MM `level` param**, same precedent `push-hack-xenia` already set for
  `channel_volume`. Currently `level` occupies a knob slot on the LEVEL
  page — freeing it gives that page's one remaining slot (`mute`) room
  to grow, or lets LEVEL absorb a currently-spare bank-1 slot instead of
  needing its own page at all.

## Audio choppiness — reported on real hardware, still open

- [ ] **Choppy audio persists across Live buffer-size changes (tried
  48kHz/512 samples).** Not yet root-caused. One concrete hypothesis was
  tested and **ruled out**: `audiosession.go`'s real-time render loop
  polls `get_param("panel_state")` every 100ms unconditionally (whether
  or not the on-screen UI is even open), on the same goroutine/thread
  that renders and writes audio — a real architectural risk in
  principle (base64-encoding a 1024-byte LCD framebuffer + JSON-building
  on the audio thread). Benchmarked directly against the real ROM: ~35µs
  mean, ~115µs worst case, vs. an 11.6ms budget for a 512-frame block —
  under 1% even at the tail. Not the cause, at least not on its own.
  **Still to check** (needs the actual push-mm.log from the affected
  session, not available to this session): the render loop already logs,
  every 2s, `blocks=... slow=... maxPre=... maxWrite=... cpu=...%%
  voices=...` plus an `xrun retries` count on stop, and a one-time
  `warning: SCHED_FIFO unavailable (rc=%d) — continuing SCHED_OTHER` if
  real-time thread priority couldn't be granted. Any of these would
  point at a real cause: high `slow` count = CPU-bound render overrun
  (plausible on its own merits — `mm-plugin` emulates two full real CPUs,
  MC68000 + DSP56300, running real Monomachine firmware, architecturally
  far heavier per sample than a simpler synthesis engine); the
  SCHED_FIFO warning = the render thread running at normal (preemptible)
  priority instead of real-time, which buffer-size tuning cannot fix.

## Out of scope for this pass

Blue LCD limited-edition SFX60 MKII exists but is a cosmetic hardware
variant, not a firmware/UI behavior difference — irrelevant to this port.
