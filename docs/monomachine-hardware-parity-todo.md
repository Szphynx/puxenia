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

- [ ] **Grid Recording is a real, separate mode gated by Record.** Per the
  Elektron quick-start guide: "You enter Grid mode by hitting the Record
  button, whereupon its red LED will light." Trig keys only edit steps
  while grid mode is active. `mm_plugin.cpp`'s `toggleStep()` currently
  taps the Trigger key directly with no Record gate at all — this was
  flagged as an unverified assumption in `monomachine-port-notes.md`
  ("no separate grid-record mode assumed") and is now confirmed wrong.
  **Fix:** hold `PanelControl::Record` for the duration of the trig tap
  in `toggleStep()` (the doc's own predicted one-line fix). Decide
  whether to enter/exit grid mode once per SEQ-page session (press
  Record on SEQ-page entry, release on exit) vs. wrap every single step
  tap — the latter is safer (matches a human's actual behavior) but
  means every step edit is 3 panel events instead of 1; profile before
  picking.
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

## Out of scope for this pass

Blue LCD limited-edition SFX60 MKII exists but is a cosmetic hardware
variant, not a firmware/UI behavior difference — irrelevant to this port.
