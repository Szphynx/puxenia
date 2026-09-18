# Monomachine hardware-parity TODO

Findings from comparing the port (`mm-plugin/`, `push-hack-mm/`, and the
`docs/mockups/push-mm-interface.html` mockup) against real Monomachine
hardware behavior, researched 2026-09-18. Sources noted per item — most of
the official Elektron/Sound on Sound/manual sites were blocked by this
session's egress proxy, so several items are search-snippet-sourced only,
not full-manual-verified. Treat unverified items the same way
`monomachine-port-notes.md` already treats its own open assumptions: don't
re-guess them, go straight to hardware/manual when available.

## Confirmed real, not yet matched in code

- [x] **Grid Recording is a real, separate mode gated by Record — gate
  added, PROVEN NOT SUFFICIENT ALONE.** Per the Elektron quick-start
  guide: "You enter Grid mode by hitting the Record button, whereupon
  its red LED will light." `toggleStep()` now holds `PanelControl::Record`
  for the duration of the trig tap (the fix this item originally
  predicted). But a real-ROM dlopen test (see `monomachine-port-notes.md`)
  shows `panel_state`'s per-step LED still reads unchanged after this
  exact call, even though the LCD content does change — so the panel
  UART is receiving *something*, but not registering a trig toggle.
  Root cause: `pressControl`/`tapControl`/`releaseControl` all happen as
  one synchronous C++ call with **zero elapsed device-time** between
  press and release, whereas `mdLibTest`'s own `tap()` helper always
  advances real device time (2048 samples) between them. This bridge has
  no render-time budget available inside a `set_param` call to do the
  same without stealing samples from the host's real-time audio
  scheduling (risks the exact kind of glitch reported after real
  hardware testing — see the audio-choppiness item below). **Still
  open**: needs a real design for giving panel taps genuine elapsed
  device-time without disrupting `mm_render_block`'s own real-time
  contract — not a one-line fix.
- [ ] **Practical effect of the above, confirmed on real hardware**: SEQ
  page pad presses currently do NOT write real steps into the pattern at
  all. The on-screen/web grid (this host's own `seqState` shadow) lights
  up as if edited, but the device's actual pattern is untouched — so
  playback (Play button) plays back whatever pattern the ROM's factory-
  default project already had, regardless of what the grid shows. This
  is very likely why "notes play with no visible steps on the grid" was
  reported.
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
