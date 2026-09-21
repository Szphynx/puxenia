# push-hub: a launcher/picker for multiple Push 3 synth hacks

Status: **proposal, not implemented.** Written for handoff to another
agent/session to build. Nothing in `push-hub/` exists yet — this doc is
the entire spec. Read `docs/environment-and-deploy.md`,
`docs/push-hack-framework-notes.md`, and `docs/monomachine-port-notes.md`
first for the framework/repo context this assumes.

## Naming convention

This repo now ships two independent synth hacks, and the user names them
**puXenia** (`push-hack-xenia`, Waldorf Microwave II/XT) and **puMMa**
(`push-hack-mm`, Elektron Monomachine). Adopt `pu<Name>` as the standing
display-name convention for any hack this hub lists, regardless of that
hack's own internal `hack.json` `id`/`name` fields — e.g. a future Virus
port would show as "puVirus." This is a display-label convention only; it
does not require renaming any existing binary, port, or directory.

## The problem

Each hack (`push-hack-xenia`, `push-hack-mm`) is a fully independent OS
process, installed and started by push-catalog like any other hack. Each
one, on its own, today:

- Opens its own ALSA seq port (`"Xenia MIDI In"` / `"Monomachine MIDI
  In"`) and **always** subscribes to Push 3's own default port for pad
  and control-surface events — this is unconditional, not gated on
  whether that hack's own UI is currently toggled on.
- Renders continuously into its own slice of `push-audio-loopback`'s
  32-channel virtual card (`ChannelOffset` in each hack's own config).
- Runs its own web UI + HTTP API on its own port (Xenia: 7707, MM: 7708).
- Binds **Shift+Device** itself (`chord.go`) to toggle its own on-screen
  takeover — calling push-manager's `SetMode(2)` (display takeover) and
  `SetMidiFilter(true)` (global MIDI intercept) directly.

`docs/push-hack-framework-notes.md` already documents the load-bearing
fact this breaks on: **ALSA sequencer subscriptions are broadcast, not
exclusive**, and push-manager's display mode / MIDI filter are each a
single global flag, not per-hack. Run two hacks at once today and:

1. Every pad press reaches **both** hacks' `Fixed()` handlers — both
   synths sound at once, no way to pick one.
2. Shift+Device is bound identically in each hack's own process — both
   see the chord and both call `SetMode`/`SetMidiFilter`, racing on the
   same global state with no coordination.

In practice this means only one hack can usefully run at a time today,
enabled/disabled via push-catalog. That defeats the point of having
multiple hacks installed. This proposal is the fix: a small always-on
hack, **push-hub**, that owns the two genuinely-exclusive resources (the
Shift+Device chord, and "which hack currently reads Push's pads and owns
the screen") while leaving everything else — audio rendering, each
hack's own web UI, each hack's own external-MIDI-gear routing — alone.

## What push-hub does

A new catalog-installed hack (own `hack.json`, own binary, no DSP/audio
of its own) that:

1. **Owns Shift+Device exclusively** as "open the hub menu." Pressing it
   always returns to (or opens) push-hub's own on-screen picker,
   regardless of which hack currently has focus.
2. **Polls each known hack's existing `GET /api/state`** (already
   implemented by both `push-hack-xenia` and `push-hack-mm` — no new
   endpoint needed for this half) to build the picker's status columns:
   reachable at all = running; `audio.ready`/`audio.message` = audio
   output state; whatever the hack's own state exposes for MIDI channel
   (Xenia: `recvChannel` under `io`; MM: `track`'s implied base channel,
   or add `baseChannel` to MM's `/api/state` if it isn't already
   surfaced plainly enough — check before assuming).
3. **Renders a menu** (top-screen tabs or a simple scrolling list, reuse
   `core/gfx`/`widgets` like every other hack's `display.go`) — one row
   per registered hack: name, ● running / ○ stopped, MIDI channel,
   🔊/🔇 audio state. D-Pad Up/Down moves a cursor, a bottom-screen
   button or Select "focuses" the highlighted hack.
4. **On focus**, calls the newly-focused hack's `POST /api/focus
   {"on": true}` and the previously-focused hack's `POST /api/focus
   {"on": false}` (if any), then gets out of the way — stops pushing its
   own frames to the display. The now-focused hack takes over the screen
   using its **own existing** `toggleUI`/`runDisplayLoop` code, unchanged.
   Shift+Device from here re-opens the hub menu (which itself calls
   `SetMode`/`SetMidiFilter` back to hub-owned) without needing to touch
   the focused hack's own display code at all.

Audio is deliberately **not** part of the focus lifecycle: every hack
keeps rendering to its own loopback channel pair regardless of focus,
same as today. Mixing independent channel pairs in Live already lets
multiple hacks' audio coexist; the only things that were ever exclusive
are the screen and Push's own pad/CC input, so those are the only two
things push-hub arbitrates.

## The contract each hack must implement

Small, additive, backward compatible — a hack with none of this still
works exactly as it does today when push-hub isn't installed.

1. **`GET /api/state` already exists on every hack.** No change required
   here unless a hack's current JSON doesn't cleanly expose a MIDI
   channel value — check each hack's actual response shape before
   assuming push-hub can read one out of it as-is.
2. **New: `POST /api/focus {"on": bool}`.** The handler does exactly
   what that hack's own `chord.go`-triggered toggle already does —
   ideally by literally calling the same internal function
   (`toggleUI`-shaped, but as a direct `setUI(on bool)` rather than a
   toggle, since two independent triggers — local Shift+Device and
   push-hub's HTTP call — must not fight over one boolean's parity).
   Refactor each hack's `uiOn`/toggle logic to expose a settable version
   both call sites use, rather than duplicating display/LED logic in a
   new code path.
3. **New: a `focused` gate on Push3-sourced input.** In each hack's
   `main.go` `Fixed()`, the branches that currently handle any
   `fromPush3` event (pad notes, control-surface CCs, D-Pad) must check
   a `focused` bool first and no-op when it's `false`. Default `true` at
   startup — a hack launched without push-hub present behaves exactly as
   it does today. Non-Push3 sources (external MIDI gear, already
   filtered by each hack's own `recvChannel` setting) are **not** gated
   by this — a hack that's unfocused for Push's own control surface
   should still respond to a MIDI keyboard plugged into "Xenia MIDI In"
   directly, if the user has one routed there; only Push's own pads/CCs
   are hub-arbitrated.
4. **Local Shift+Device must stand down when push-hub is present.**
   Simplest check: at startup, each hack looks for push-hub's known
   local port (e.g. a quick `GET http://localhost:<hub-port>/api/ping`
   with a short timeout) and, if it answers, skips binding its own
   Shift+Device handler in `chord.go` for the rest of that run (falls
   back to hub-driven focus only). If push-hub isn't running, behave
   exactly as today. Poll once at startup, not continuously — if the
   user installs push-hub after a hack is already running, that hack
   needs a restart to pick it up; document this rather than building a
   live re-check for a one-time install-order edge case.

## Registry

A flat, static list is enough for two hacks and is the right rung on the
ladder until a third one makes it annoying — **do not** build dynamic
push-catalog directory scanning for v1. Something like:

```json
// push-hub/hacks.json
[
  { "label": "puXenia", "api": "http://localhost:7707", "process": "push-xenia" },
  { "label": "puMMa",   "api": "http://localhost:7708", "process": "push-mm" }
]
```

Upgrade path, not required now: read push-catalog's own install
directory listing instead of a hand-maintained file, once there are
enough hacks that editing this by hand is the actual friction — not
before.

## Generalizes to other/future projects? Yes, cleanly

Any future hack (a Virus port, a Rytm port, whatever) needs exactly:
one entry in `hacks.json`, and the two small additions in the contract
above (`/api/focus` + the `focused` gate). Nothing about push-hub itself
is Xenia/MM-specific — it never touches DSP, audio, or a hack's own
parameter model, only the two genuinely shared/exclusive resources
(screen, Push's own control-surface input). A hack that skips the
contract entirely still runs standalone exactly as it does today; it
just can't be focus-arbitrated by the hub (pushing it while another hack
also has focus would reproduce today's collision) — worth a one-line
warning in push-hub's own log if a registered hack never responds to
`/api/focus`.

## Suggested layout (mirrors `push-hack-mm`'s own structure)

```
push-hub/
  hack.json
  hacks.json          # static registry, see above
  build.sh  deploy.sh  install_push_hub.sh   # same shape as push-hack-mm's
  src/
    main.go            # supervisor boilerplate, ALSA port, Shift+Device chord
    registry.go         # loads hacks.json, polls each /api/state
    focus.go             # focus/unfocus dispatch (POST /api/focus calls out)
    display.go            # menu render + push-manager display calls
    ui/index.html           # optional: same picker, browser-side
```

Per-hack patch (both `push-hack-xenia` and `push-hack-mm`):
- `webserver.go`: add `handleFocus`.
- `main.go`: add a package-level `focused` bool (atomic or mutex-guarded,
  same pattern as `uiOn`), gate `Fixed()`'s `fromPush3` branches on it.
- `chord.go`: at startup, probe for push-hub; skip local Shift+Device
  binding if present.
- `display.go`: split `toggleUI`'s toggle into a `setUI(on bool)` both
  `chord.go` and the new `handleFocus` call.

## Open questions for whoever builds this

- Exact MIDI-channel field to read from each hack's `/api/state` for the
  picker's channel column — verify against each hack's actual current
  JSON shape rather than assuming a name.
- Should push-hub persist last-focused hack and auto-focus it on its own
  boot? Nice-to-have, not required for v1.
- Menu input while the hub screen is showing: pads vs top-screen
  buttons vs D-Pad for cursor movement — pick one, document why, keep it
  consistent with how `push-hack-xenia`/`push-hack-mm` already use each
  control so muscle memory transfers.
- No auth on any of this (matches every existing hack's own HTTP API) —
  fine as long as everything stays bound to localhost; flag loudly if
  that ever changes.
