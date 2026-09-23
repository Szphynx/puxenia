# TODO — feature requests and open work, indexed by topic

Cross-project list, not tied to one hack. Add new items under the
matching topic (create a new topic heading if none fits) rather than a
flat chronological list, so this stays useful as it grows. Each hack's own
more detailed/technical TODOs stay in their own doc (e.g.
`docs/monomachine-hardware-parity-todo.md`) — this file is for
user-facing feature requests and known-buggy areas across the whole
project.

## push-hub: process control

- **Restart/Quit buttons — implemented, not yet hardware-verified.**
  - Hub menu: a 3rd per-row action, RESTART (bottom-screen button 3,
    alongside FOCUS/START-STOP) — stops then starts the selected hack
    (`push-hub/src/focus.go`'s `restartHack`), for the case START/STOP
    alone can't fix (a hack running since before push-hub started never
    re-probes for it — see `hubPresent`'s doc in each hack's own
    `chord.go` — and keeps fighting the hub for the screen until
    restarted).
  - Hub menu, far right (buttons 7/8, not tied to the cursor row):
    RE-HUB (restarts push-hub itself — no supervisor process exists for
    it the way `push-hack-xenia`/`push-hack-mm` have, so this spawns a
    detached replacement that waits `push-hub/src/focus.go`'s
    `restartSelfDelay` before binding the port, then quits this
    instance) and QUIT (push-hub's own graceful shutdown, via
    `focus.go`'s `quitSelf`).
  - Each hack's OWN on-screen SETTINGS page also got QUIT/RESTART
    (`push-hack-xenia`/`push-hack-mm`'s new `selfcontrol.go`), so you
    don't have to leave the hack's screen and go back to the hub first —
    "per device including the hub," per the explicit ask. These lean on
    the supervisor/child split `main.go`'s `runSupervisor` already
    implements: RESTART is just the child's own normal shutdown+exit
    (the supervisor sees an unprompted exit and always respawns, same as
    a crash); QUIT signals the supervisor's own PID directly
    (`os.Getppid()`) so the supervisor takes its "don't respawn" path
    instead.
  - **Not yet re-verified on real Push hardware** (this environment has
    no Push/push-manager to test against) — see each file's own doc
    comment for the reasoning. Confirm on next hardware pass: hub-side
    RESTART/RE-HUB/QUIT all actually fire from their bottom-screen slots
    (3/7/8) without colliding with anything else on that row; each
    hack's own QUIT/RESTART (SETTINGS page, slots 4/6 for MM at
    `CCScreenBot4`/`CCScreenBot6`, same CCs for Xenia at slots 3/5 — the
    slot **index** differs between the two because Xenia's SETTINGS page
    had no bottom strip drawn at all before this, while MM's already had
    one with different slots occupied by its own SET/EXIT buttons) don't
    collide with the existing per-column SET commit buttons on that same
    page.

## Presets

- **Save preset** — neither `push-hack-xenia` nor `push-hack-mm` can
  currently save the live/edited sound back to a slot. Xenia has no
  concept of "save" implemented at all (`params.go`'s PRESETS page is
  browse + Load only, see `loadStagedPreset`); the underlying device-side
  mechanism (a real Store SysEx, `SysexCommand::SingleStore` per
  `xtMidiTypes.h`) is not yet wired up on the C++ side either. Needs a
  design pass: which slot does "save" target (currently-loaded program?
  a chosen destination?), and a real hardware-tested Store command before
  shipping it as working (same rigor as the rest of
  `docs/xenia-gearmulator-notes.md`'s SysEx work — don't guess the byte
  layout, check the reference JUCE plugin's packet template first).

## Display: show real names for enum/mode parameters, not raw numbers

Every param that's really a named mode (Xenia's `filter_type`,
`osc2_sync`, `lfo1_shape`, `arp_direction`, etc. — anything with a small
enumerated range rather than a continuous 0-127 sweep) currently displays
as a bare number on-screen and in the web UI, the same as a plain
continuous value. Requested: these should show their actual mode name
(e.g. "LP 24dB" instead of "3" for `filter_type`).

Current state per hack:
- **push-hack-xenia**: `xenia_plugin.cpp`'s `chain_params` JSON always
  reports `"type":"int"` for every param (see `xenia_get_param`'s
  `chain_params` handler) — there's no `"enum"`/`"options"` path at all
  server-side, unlike `push-hack-mm`'s Braids-style params which do
  support it (`params.go`'s `paramMeta.Options`/`formatValue`'s enum
  case). Needs: (1) mark the relevant `kParams` entries as enums with a
  real options list (the actual mode names, from the Microwave XT
  manual's parameter tables — same "transcribe from the real chart, don't
  guess" discipline as the rest of this project) in `xenia_plugin.cpp`,
  (2) confirm `chain_params`'s JSON builder emits `"type":"enum"` +
  `"options":[...]` for them the same shape `params.go`'s `fetchChainParams`
  already expects (it already has special-case handling for an enum's
  missing min/max — see that function's own comment on the "engine stuck
  on CSAW" bug this fixed for Braids/mm — so the Go side should already
  handle it correctly once the plugin actually sends it).
- **push-hack-mm**: partially there already — confirm every relevant MM
  param (LFO shapes, trig conditions, etc.) is actually marked as an enum
  with real option names, not just the ones already known to work: audit
  `mm-plugin`'s param table the same way, not just spot-check.

## Known bugs (fixed, unverified on real hardware) — see the relevant doc for detail

- push-hub title clipped, FOCUS bouncing to Ableton's screen, START doing
  nothing: fixed, see `docs/push-hub-proposal.md`'s "Deviations from this
  proposal" section.
- Hub can't reclaim the screen from a focused hack ("fights between xenia
  and the hub endlessly"): fixed — `onChordCC` (push-hub's `chord.go`)
  now explicitly defocuses every hack before reclaiming, instead of only
  ever setting the hub's own state and leaving the previously-focused
  hack believing it still owned the screen. See `focus.go`'s
  `defocusAll`.
- A freshly-started hack (e.g. via push-hub's own START button) defaulted
  `focused=true` before ever being explicitly focused, so it was
  immediately audible and fought the hub for the screen the moment its
  own UI opened. Fixed — `focused` now defaults `false` when a hub is
  detected at boot (each hack's `main.go`), `true` only in the
  hub-absent/standalone case.

## Open, NOT yet diagnosed

- **puMMa: doesn't start, can't be focused, at all.** Not yet root-caused
  — everything checked so far (the `/api/focus` contract, the hub-probe
  code, `hacks.json`'s `dir`/`exec`/`process` fields for the direct-start
  fallback) matches `push-hack-xenia`'s own working equivalent
  byte-for-byte, so this isn't an obvious code-level divergence between
  the two hacks. Leading suspicion: `/tmp` is tmpfs and gets wiped on
  every Push reboot (see `docs/audio-pipeline-debugging.md`'s directive
  on this) — if the Push has rebooted since puMMa was last deployed, or
  if a recent session only ran `deploy-all.sh --hub` (deploying push-hub
  alone, not puMMa), `/tmp/mm-hack` may simply be empty or stale on the
  device. **Next step**: run a full `deploy-all.sh` (or at least
  `--mm`) to guarantee puMMa is actually present and current on the
  device, then retry; if it still doesn't start, this needs
  `ssh ... tail -f /tmp/mm-hack/push-mm.log` (or push-hub's own log, to
  see the actual error `setServiceRunning`'s fallback returned) — this
  can't be diagnosed further from source alone without that log.
