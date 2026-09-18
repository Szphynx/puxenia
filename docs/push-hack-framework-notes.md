# push-hack framework (push-manager / push-display / push-catalog) notes

Findings about the upstream `ableton-push-hack` framework
(`github.com/federico-pepe/ableton-push-hack`) gained while integrating
push-xenia with it — not duplicated from that repo's own docs.

## push-manager and push-display are two separate, both-required pieces

A common assumption to avoid: "push-manager" sounds like it should be the
whole story for on-screen UI + MIDI control. It isn't.

- **push-manager** (Go daemon, port 7701): a web file browser, a MIDI
  *monitor* (broadcast ALSA subscription, read-only), and the HTTP API
  (`/api/display/*`) that other hacks poll to check display availability
  and write frames through. It does **not** itself draw anything on
  Push's screen or gain any exclusive control over MIDI.
- **push-display** (`push_hook.so`, an `LD_PRELOAD` hook injected into
  Push3's own process): the piece that actually intercepts
  `libusb_bulk_transfer` to draw on the screen, and can neutralize MIDI
  into Live via a shared-memory flag (`midiflt`). **Without this
  installed and loaded, nothing draws on-screen, no matter how correct a
  hack's own display-writing code is**, and nothing can claim exclusive
  MIDI control.
- **push-catalog** (port 7702): an app-store-style installer for
  separately-published hacks. Unrelated to display/MIDI; only listed here
  because it's the third piece in the framework's own `hacks/` directory
  (as of this writing, those three are the *entire* catalog — there's no
  large library of hacks to "install everything" from).

If a hack's on-screen UI shows nothing and `ps aux | grep -iE
"push-manager|push-display"` comes back empty, that's the actual root
cause — not a bug in the hack's own rendering code.

## push-display's shared-memory files need directory permissions the
## generic install script doesn't grant

`push_hook.c`'s constructor `open()`s two world-writable (0666) files —
`framebuf` and `midiflt` — with `O_CREAT`, inside
`/data/push-hack/hacks/push-display/`. That directory gets created by
the generic install flow as **root, mode 755**. Push3 itself (and
therefore the hook loaded into it) runs as **`ableton`, uid 1000** — which
cannot create a file inside a 755 root-owned directory. Result: both
`open()` calls fail silently, logged only as `shm: open failed: ...` /
`midi_filt: open failed: ...` in the hook's own log — easy to miss, and
push-manager's `/api/display/status` will happily report `connected:
false` with no more specific error.

**Fix**: `chmod 777 /data/push-hack/hacks/push-display/` after deploying
the `.so`, then restart Push3 so the hook's constructor runs again and
actually creates the files. Confirmed working: after the chmod + restart,
the hook's log shows `shm: initialized at .../framebuf — ...` and
`midi_filt: initialized at .../midiflt (enabled=0)`, and
`curl http://<push-ip>:7701/api/display/status` returns `"connected":
true`.

This looks like a real gap in the framework's own install tooling (the
directory permissions it creates don't match what the hook needs), worth
fixing upstream rather than re-discovering per-project.

## push-display's own log often isn't where its docs say

The hook's constructor tries `/data/push-hack/logs/push-hook.log` first,
falling back to `/tmp/push-hook.log` if that `fopen(..., "a")` fails.
That first path gets created **as root** (mode 644) by whichever process
first triggers the constructor while running as root (e.g.
`start-stop-daemon` itself, before it drops privileges and execs the
actual Push3 launcher). Once Push3 (running as `ableton`) loads the same
`.so`, its own `fopen` on that root-owned 644 file fails — group/other
have no write bit — and it silently falls back to `/tmp/push-hook.log`
with zero indication in the "official" log location.

**Practical upshot**: if `/data/push-hack/logs/push-hook.log` only ever
shows one boot-time line and never updates, **check
`/tmp/push-hook.log` instead** before concluding the hook isn't running.

## ALSA sequencer subscriptions are broadcast, not exclusive

Every hack that "reads Push's MIDI" (`core/alsaseq`'s `Client.Subscribe`)
gets a full copy of every event on that port — subscribing is not the
same as claiming ownership. Ableton Live's own driver and any number of
hacks can all be subscribed to the same port simultaneously and all
receive the same messages. There is **no MIDI arbitration at the ALSA
layer at all** in this framework. The *only* mechanism that can stop Live
from also seeing an event is push-display's `midiflt` shared-memory flag
(`enabled=1` makes the hook overwrite each `snd_seq_event`'s type with
`SND_SEQ_EVENT_NONE` before Live's driver reads it) — SysEx and Active
Sensing always pass through regardless. If two things react to the same
knob/pad, this is why, and the fix is exclusively at the push-display
layer, not by changing how a hack subscribes.

## Push 3 hardware MIDI facts worth knowing before implementing a
## handler for them (not obvious from `core/push3`'s constant names alone)

- **The touch strip is real Pitch Bend**, not a CC. It's
  `alsaseq.EvPitchBend` (event type 13, distinct from `EvController`'s
  type 10) on MIDI channel 1 (nibble value 0), plus a separate Note
  On/Off marker (`push3.NoteTouchStrip = 12`) for touch start/end. A
  handler that only switches on `EvController`/`EvNoteOn`/`EvNoteOff`
  silently drops it entirely — no error, just nothing happens.
- **The dedicated hardware Volume knob is `push3.CCVolume = 79`** — a
  relative encoder like the 8 param encoders (71-78), but *not* one of
  them; it needs its own case in a CC switch, and won't collide with the
  71-78 range check.
- **Held pads carry per-pad MPE channels**, not just channel 0. Each
  held pad gets Channel Pressure + CC74 ("timbre"/Y-axis) on its own
  dynamically-assigned channel — which is *inside* the CC71-78 range used
  for the 8 on-screen param encoders. Any CC handler for those encoders
  MUST filter to channel 0 first, or a held pad reads as a param encoder
  spinning wildly (misdecoding an absolute 0-127 MPE value as a relative
  encoder delta). This also means: a "restrict MIDI to one channel"
  feature must never apply to Push's own pad-grid notes, only to
  external MIDI sources — filtering pads by a fixed channel would silence
  most of the grid.
- The pad grid is notes 36-99 (`push3.PadNoteMin`/`PadNoteMax`), 8×8,
  `push3.PadNote(col, row)` / `PadCoord(note)` convert between the two.

## ALSA seq event data layout (relevant when writing a `Handler.Fixed`)

The 12-byte data union (`snd_seq_ev_ctrl`) is: `data[0]` = channel
(low nibble), `data[4:8]` = param (LE uint32, the CC number for a
Controller event, unused/0 for Pitch Bend), `data[8:12]` = value (LE,
**signed** for Pitch Bend: -8192..8191; unsigned 0-127 masked with
`&0x7F` for a normal CC). Getting the signedness wrong on Pitch Bend's
value field is an easy silent bug — Go's `binary.LittleEndian.Uint32`
returns unsigned, so it needs an explicit `int32(...)` cast to read
correctly.
