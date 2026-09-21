# puMMa troubleshooting directives

Everything learned the hard way while building, deploying, and debugging
`push-hack-mm`/`mm-plugin` on real Push 3 hardware — real bugs found via
empirical testing against the real ROM, real deploy/ops gotchas, and the
mistakes worth not repeating. Written as directives for whoever (human or
agent) touches this code next. Cross-references `monomachine-port-notes.md`
and `monomachine-hardware-parity-todo.md` rather than duplicating their
content — this doc is the "how we found things out" companion to those.

## The single most valuable technique this whole project: empirical proof, not guessing

Every non-trivial bug in this codebase was found — and every fix was
verified — by building a small standalone `dlopen` harness that loads
`libmm-plugin.so` directly, drives it against the **real** ROM (not a
stub), and asserts an observable before/after state change via
`get_param`. Not by reading code and reasoning about what "should" happen.

**Directive: before claiming any fix works, or any hypothesis is true,
write the harness and check it against the real ROM.** This caught real,
non-obvious things reasoning alone would have missed or gotten backwards:
- A boot-gate bug that "reasoning" would have called `create_instance`
  returning cleanly and being ready — it wasn't, for ~22s.
- A grid-record fix that looked complete after step 1 (adding the Record
  hold) but was proven, by testing, to still not register — needed a
  second, deeper fix (deferred releases).
- Play/Stop buttons turned out to work fine with zero elapsed device-time
  between press/release, while Trigger keys did NOT — these looked like
  the same kind of control and are not. Don't generalize a finding about
  one panel control to all panel controls without testing each one.
- MIDI realtime forwarding (Start/Stop) was confirmed architecturally real
  (found in the emulator's own source) but empirically proven to have NO
  effect by itself — a plausible-sounding feature that doesn't actually
  work yet. Reasoning from "the code path exists" to "therefore it works"
  would have been wrong.

A negative result (proven "this doesn't work") is just as valuable as a
positive one — write it down, don't paper over it.

## Real firmware/emulation behavior (mm-plugin / DSP56300+68k JIT)

- **The emulated firmware needs ~20s of real device-time to boot before it
  responds to ANY MIDI or panel input at all.** Confirmed via
  `gearmulator-md-mm`'s own `mdLibTest` suite requiring
  `advance(hardware, g_samplerate * 20)`. Anything sent before that is
  silently dropped by the firmware itself — not a bug in the C++ bridge,
  a real hardware-accurate boot sequence. `mm_plugin.cpp` gates on this
  (`MmInstance::framesSinceCreate`/`isBooting`); if you add a new
  device-facing call path, make sure it's covered by that same gate.
- **A synchronous press-then-immediate-release of a panel button is NOT
  guaranteed to register with the firmware's key-scan.** Confirmed for
  Trigger keys (grid step editing) — needed real elapsed device-time
  between press and release, implemented via a deferred-release queue
  (`MmInstance::pendingPanelReleases`, drained once per `mm_render_block`,
  2048 frames matching `mdLibTest`'s own `tap()` helper) rather than
  stealing samples synchronously inside `set_param`. **But this was NOT
  true for Play/Stop/Record**, which registered fine at zero elapsed
  time. Don't assume — test each control.
- **Never mutate Go-side "shadow" state in the same breath as an
  unconfirmed device-facing call.** Every `set_param`/`on_midi` call is
  fire-and-forget with no acknowledgment path. If the underlying call can
  ever be silently dropped (boot window, a firmware quirk) and the Go
  side updates its own local state unconditionally regardless, the two
  will permanently diverge for the rest of that session with no way to
  detect it — this was the actual root cause behind three different
  user-facing symptoms that looked unrelated ("parameters don't work,"
  "pads select the wrong track," "mutes don't work"). Fix: gate local
  state mutation on the exact same readiness condition that gates the
  device call (see `audiosession.go`'s `drainCtl`: drops the whole
  control event during boot rather than applying half of it).
- **MIDI CC/page/index mappings must come from the real source
  (`mdautomation.cpp`'s `mmController`), not be guessed or assumed** —
  verified byte-for-byte against `parameterDescriptions_mm.json`. This
  checked out correctly on the first pass; don't skip re-verifying it if
  the param table ever changes.
- **ROM validity is a computed fingerprint (FNV-1a 64-bit over the full
  8MiB image), not a filename/size guess.** `md::RomLoader::isRomForModel`
  is the actual gate; compute and compare it directly when validating a
  ROM rather than trusting that a file "looks right."
- **MIDI realtime bytes (Clock/Start/Continue/Stop, 0xF8-0xFF) are a real,
  deliberately-modeled hardware feature in the emulator** (confirmed in
  `mdhardware.cpp`: correct 1-byte wire-length computation for
  status≥0xF0, a dedicated `pumpRealtime()` path letting these bytes cross
  ahead of other traffic) — but forwarding them is NOT sufficient by
  itself to make the sequencer respond. Real Monomachine almost certainly
  needs its Global Clock Source switched from Internal to External first;
  how to do that from MIDI/SysEx is still unresearched. Don't ship
  "forwards the bytes" as "syncs to external clock" — they're different
  claims.

## Go host / threading discipline

- **Every `bridge_plugin_*` call must happen on exactly one goroutine**
  (the audio render loop, `audioSession.run`) — the C++ plugin instance
  is not thread-safe. Every other goroutine (display loop, web server,
  ALSA read loop) only ever writes to a channel or reads a
  mutex-guarded shadow. This was already established discipline before
  this session; it must never be violated when adding new features —
  every new control path added this session (base channel web endpoint,
  MIDI realtime forwarding, deferred panel releases) was built to respect
  it.
- **`panel_state`'s cost is negligible — don't blame it for perf issues
  without measuring.** Benchmarked directly: ~35µs mean, ~115µs worst
  case, against an 11.6ms budget for a 512-frame block. Ruled out as a
  choppy-audio cause with real numbers, not assumption.
- **Real audio-choppiness diagnosis needs the actual log** (the
  `progress:` lines' `slow=` count, and whether `SCHED_FIFO unavailable`
  ever printed) from the affected session — this cannot be diagnosed by
  reasoning about the code alone. If a user reports intermittent glitches,
  ask for these specific log lines before hypothesizing further.
- **Web UI and physical-pad input paths should share the exact same
  control-event types**, not parallel implementations — this is why every
  pad-grid fix this session (grid-record gate, deferred releases) applied
  equally to `webserver.go`'s `/api/seq/*` handlers with zero extra work.
  When adding a new physical-control feature, check whether the web UI
  needs the same capability exposed (base channel was missing from the
  web UI for a while — found only by an explicit endpoint-by-endpoint
  audit against puXenia, not by inspection alone).

## Deploy/ops gotchas (the console-command failures, not the code)

- **`ssh host "cmd with | inside"` does not do what you think.** The
  local shell's quoting doesn't survive to the remote shell as one
  token — ssh joins argv with spaces when forwarding, so an unescaped
  `|` becomes a real pipe on the remote end. Always wrap the ENTIRE
  remote command as one single-quoted (or fully backslash-escaped)
  string, e.g. `ssh host 'grep -E "A|B" file'`, not
  `ssh host grep -E "A|B" file`.
- **`pkill -f <name>` can kill its own invocation.** When run as a
  one-shot `ssh host "pkill -f push-mm"`, that executes remotely as
  `bash -c 'pkill -f push-mm || true'` — and that wrapping bash's own
  command line contains the literal substring "push-mm", so `-f` (which
  matches full command lines) can match and kill the very shell running
  it, before it ever reaches `|| true`. This drops the ssh session and
  can silently abort a `set -e` deploy script with no visible error.
  **Use `-x` (exact process-name match) instead** — it only matches the
  actual target binary's name, never the wrapping shell.
- **The supervisor/child pattern means two processes share one name.**
  `push-mm` re-execs itself (`main.go`'s `runSupervisor`) as a child with
  the same `os.Args[0]` — so `pkill -x push-mm` correctly kills both at
  once (this is desired: both dying together means the supervisor sees
  its own signal directly and does NOT respawn, which only happens when
  the child alone dies unexpectedly).
- **push-manager is a separate, always-running service with its own
  persistent state** (display mode, MIDI filter) — it does not get reset
  just because `push-mm`'s process exits. If `push-mm` ever dies without
  running its own graceful-shutdown handler (a hard kill, a crash), Push
  stays stuck in on-screen takeover / MIDI-intercept mode with nothing
  left alive to release it. Always pair "kill the hack" with a direct
  call to push-manager's own API (`POST /api/display/mode {"mode":0}`,
  `POST /api/midi/filter {"enabled":false}`) as a safety net — see
  `stop.sh`. Don't rely solely on the hack's own signal handler.
- **File permission `000` looks like a normal "permission denied" but
  `chown` alone won't fix it** — check `ls -la` for the actual mode
  bits, not just the owner, before reaching for `chown`. `chmod` is
  usually the actual fix.
- **This repo's puMMa work lives on a feature branch
  (`sph/busy-goldberg-ajmuyj`), not `main`.** `git pull origin main`
  (the existing puXenia muscle-memory command) will NOT show any of it
  until/unless that branch is merged. Always check which branch has the
  code you expect before assuming a pull "should have" picked something
  up.
- **The real ALSA card ID is whatever's in `/proc/asound/cards`'
  brackets (the short ID), not the long description text.** Both
  `push-hack-xenia` and `push-hack-mm` at one point hardcoded a card ID
  string ("PHVAudio") that only ever appeared in the long description
  ("Push Hack Virtual Audio") — the real short ID was "Audio". This had
  already been found and fixed for Xenia on `main` before `push-hack-mm`
  was forked from an older copy of that file; the fork carried the stale
  value forward. **When hardcoding a device/card identifier, verify it
  against the live system directly** (`cat /proc/asound/cards`), and
  when copying a pattern from a sibling hack, check whether that sibling
  has since fixed something the copy predates.

## UX lessons from real hardware testing

- **Push's pad grid is numbered bottom-up (row 0 = bottom)** — this is
  easy to get backwards relative to how a user reads/describes rows
  top-down ("the first row," "the second row"). When placing a
  deliberately-unused/reserved row in a grid that doesn't use all 8 rows,
  put it at a physical edge (top or bottom), never sandwiched between two
  functional rows — a dead row in the middle reads as "broken," the same
  dead row at the edge reads as "unused margin." Found only after a real
  user described "the second row is useless" — which was, in fact,
  correct and unbound by design, just placed confusingly.
- **A control surface's optimistic visual feedback becomes actively
  misleading once you know the underlying device state can silently
  diverge from it.** Don't just "add a light that turns on when pressed"
  — trace whether that light's condition is actually confirmed by the
  device, or just a hopeful local guess.
