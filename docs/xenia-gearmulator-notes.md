# Xenia / gearmulator (Waldorf Microwave II/XT) notes

Findings specific to the actual synth engine this plugin wraps — gained by
reading `gearmulator`'s source directly
(`charlesvestal/gearmulator/source/waldi/xt/xtLib`) and by offline testing,
not duplicated from that repo's own code comments.

## This is a full firmware emulator, not a reimplementation

`xtLib` runs the **actual Microwave XT firmware binary** (ROM images:
`MWXT_FW_IC_1.BIN` + `MWXT_FW_IC_2.BIN`) on an emulated 68k MCU + DSP56300
DSP. This has one big practical consequence: **MIDI CC dispatch logic
lives inside the compiled ROM's machine code, not in any inspectable C++
source.** `grep`-ing `xtLib` for `ControlChange`, `0xB0`, `handleCC`, etc.
returns nothing at all — that logic genuinely doesn't exist anywhere as
readable source in this codebase. Don't waste time searching xtLib's C++
for "why doesn't CC50 do anything" — the answer, if findable at all
without a hardware MIDI implementation chart, is empirically testable via
`xenia_render` (below), not readable in source.

What *is* readable and useful: `xtMidiTypes.h`'s `GlobalParameter` enum
(exact order below) and `xtState.cpp`'s factory-reset function, which
shows every global parameter's real default value at boot.

```cpp
enum class GlobalParameter {
    Reserved0, Version, StartupSoundbank, StartupSoundNum, MidiChannel,
    ProgramChangeMode, DeviceId, BendRange, ControllerW, ControllerX,
    ControllerY, ControllerZ, MainVolume, Reserved13, Reserved14,
    Transpose, MasterTune, DisplayTimeout, LcdContrast, Reserved19,
    Reserved20, Reserved21, Reserved22, StartupMultiNumber,
    ArpNoteOutChannel, MidiClockOutput, ParameterSend, ParameterReceive,
    InputGain, Reserved29, Reserved30, Reserved31
};
```

(0-indexed — `ParameterReceive` is index 27, `ParameterSend` is 26,
`ControllerW/X/Y/Z` are 8-11.)

**`ParameterReceive` (index 27) defaults to `1` (on) at boot** —
confirmed directly from `xtState.cpp`'s reset function
(`setParam(GlobalParameter::ParameterReceive, 1); // on`). A SysEx fix
that force-sets this to 1 at boot (which this project shipped at one
point, `xenia_plugin.cpp`'s `sendGlobalParameter(inst, 27, 1)` at
instance creation) is **provably a no-op** — it was a reasonable
hypothesis for "CC doesn't audibly change the sound while Program Change
does," but it's already on by default, so it isn't the actual cause.
**The real cause of that symptom is still open** as of this writing — see
"Known open issue" below. Don't re-derive the ParameterReceive theory
from scratch; it's already ruled out.

`ControllerW/X/Y/Z` (indices 8-11) are worth investigating next if
picking this up: on Waldorf-era synths of this generation, several
modulation destinations are only reachable through 4 generic assignable
controller slots (each mapped to an arbitrary incoming MIDI CC number via
these 4 global params), which the **currently loaded patch's own
modulation matrix** then has to route to an actual destination (e.g.
"Controller X → Filter Cutoff, depth 64") for that CC to have any
audible effect at all. If the loaded factory patch's mod matrix doesn't
happen to route whichever controller slot a given CC lands on, turning
that CC does nothing — which would exactly match the reported symptom
(cutoff/resonance CCs silently do nothing, while Pitch Bend and Program
Change — both hardwired, not mod-matrix-routed — work fine). This is a
plausible next lead, not yet confirmed.

## The real MIDI CC chart, transcribed once — don't re-guess it

The `kParams` table in `xenia_plugin.cpp` was built by transcribing the
**real** "MIDI Controller Assignments" chart (Waldorf Microwave 2/XT/XTk,
software release 2.28) that the project owner provided directly — not
guessed from "conventional" synth CC assignments. In particular:
**cutoff = CC50, resonance = CC56** — not the CC74/71 that most other
synths' MIDI implementations use by convention (an earlier, wrong first
guess in this project's history, since corrected). If cutoff/resonance
still don't work after checking the transcribed table, the table itself
was already double-checked against the real chart once; look elsewhere
first (see "Known open issue").

**Channel 10 (0-indexed 9) is the only channel the ROM's default "Play
Sound" boot patch responds to** — confirmed empirically, not from any
spec. `xenia_plugin.cpp`'s `kWorkingChannel0Indexed = 9` applies this to
every outbound message (notes, CC, Program Change, SysEx broadcast DEV
byte aside). If a future patch/multi-mode setup needs a different
channel, this constant is where it's hardcoded — it's not derived from
any global parameter automatically.

## `xenia_render`: offline WAV-render test harness — much faster than a
## full Push deploy cycle for testing "does this CC actually do anything"

Already built in this project's dev environment (see
`environment-and-deploy.md` for the general build setup) at
`~/xenia-build/build/xenia_render`. Renders a MIDI script straight to a
WAV file with **no Push 3, no Ableton Live, no plugin_api_v2 host bridge
involved at all** — the single fastest way to test a CC/parameter
mapping in isolation.

```
xenia_render <rom_dir> <script.txt> <out.wav>
```

Script format (see the tool's own header comment for the authoritative
version):
```
# comment
<time_ms> on <note> <vel> [ch]      note-on (channel 1-16, default 1)
<time_ms> off <note> [ch]           note-off
<time_ms> pc <prog>                 program change
<time_ms> cc <controller> <value> [ch]
render_seconds <float>              total render length (default 5)
```

### Sharp edge: pass ABSOLUTE paths for the script and output file

`xenia_render`'s `main()` does `chdir(romDir)` **before** opening the
script file (`xenia_render.cpp:197`, ahead of the `parseScript` call at
:212). If `<script.txt>` is given as a relative path, it resolves
relative to the ROM directory it just `chdir`'d into, not the directory
you ran the command from — producing a flatly misleading `[xenia_render]
cannot open script: <name>` even though the file plainly exists (`ls`,
`stat`, `cat` all succeed on it) with completely normal permissions.
**This one cost real debugging time** — every "is the file corrupted /
is this a WSL filesystem quirk / hidden character in the filename"
avenue was a dead end; the actual bug is the tool's own `chdir` ordering.

**Fix**: always pass absolute paths for the script and the output WAV:
```
./build/xenia_render rom /home/<user>/xenia-build/my_script.txt /home/<user>/xenia-build/out.wav
```
(A relative *ROM* directory argument is fine — that's the one the tool
itself expects to resolve relative to the original CWD, before its own
`chdir`.)

### Two ready-made sweep scripts for cutoff/resonance debugging

Already written and known-good format (adjust paths to absolute per
above before running):

```
# cutoff_sweep.txt — program 0, ch10, cutoff CC50 stepped 0→127
render_seconds 9
0     pc 0 10
100   on 60 100 10
200   cc 50 0 10
2000  cc 50 20 10
4000  cc 50 60 10
6000  cc 50 100 10
8000  cc 50 127 10
8500  off 60 10
```

```
# resonance_sweep.txt — cutoff pinned mid-low, resonance CC56 stepped 0→125
render_seconds 9
0     pc 0 10
100   on 60 100 10
200   cc 50 40 10
300   cc 56 0 10
2000  cc 56 40 10
4000  cc 56 80 10
6000  cc 56 110 10
8000  cc 56 125 10
8500  off 60 10
```

Open the resulting WAVs in any spectrogram viewer (Audacity, etc.): if
the CC is actually reaching the filter, brightness/resonance emphasis
should visibly/audibly change across the render. If it's flat throughout,
that's hard proof the CC isn't reaching the filter at all — which then
justifies digging into the `ControllerW/X/Y/Z`-and-mod-matrix theory
above, or requesting the real Microwave XT service manual's MIDI
implementation chart for the exact CC dispatch rules.

## The real answer: most parameters aren't CC-addressable at all — they
## need SysEx Single Parameter Change (0x20)

The device's own official **MIDI Implementation Chart** (the standard
one-page MIDI spec doc, distinct from the SDATA table below — get this
from the same manual/PDF source if it's needed again) settles this
definitively: only **7 CCs are directly recognized** — Modwheel(1),
Breath(2), Portamento Time(5), Volume(7), Pan(10), Bank Select(32),
Sustain(64) — with a footnote reading *"See MIDI Controller Assignments
for more information"* for everything else. `kParams`' CC numbers for
cutoff/resonance/etc. (50, 56, ...) came from a *different* document
("MIDI Controller Assignments") that — in hindsight — was very likely
describing the 4 generic assignable-controller slots
(`ControllerW/X/Y/Z`, global params 8-11) and a *patch's own modulation
matrix* routing them, not a direct hardwired CC-to-parameter mapping.
Sending a raw CC in that range can land on a device receiving that CC as
a generic mod source with **no patch routing behind it at all**,
producing exactly the observed symptom (silently no-op) regardless of
which CC number, which patch, or which of the two filters is targeted —
confirmed by testing all of those independently and getting the same
flat result every time (see "Everything already ruled out" below).

The actual mechanism for real-time parameter edits is a separate SysEx
command, confirmed directly against `xtMidiTypes.h`/`xtState.cpp`:

- **Command**: `SysexCommand::SingleParameterChange = 0x20` (`xtMidiTypes.h`)
- **Wire format**: `F0 3E 0E DEV 20h LOC IH IL XX F7`
  - `DEV` — device ID; `0x7F` (`wLib::IdDeviceOmni`) is a real, defined
    broadcast constant, not a guess.
  - `LOC` — the byte right after the command. For the two "live edit
    buffer" location enum values (`SingleEditBufferSingleMode` in Single
    mode, `SingleEditBufferMultiMode` in Multi mode — `xt::State`
    chooses between them itself based on `isMultiMode()`, **not** from
    this byte), the byte's actual value only matters in Multi mode (it's
    an index into `m_currentMultiSingles`, and an out-of-range value gets
    silently rejected — `getSingle()` returns `nullptr`). **Use `0x00`**,
    valid in both modes; a "clever" nonzero value like the raw
    `SingleEditBufferSingleMode` enum constant (`0x20`) can be
    out-of-bounds in Multi mode and cause a silent, undebuggable no-op
    (this cost real time before the byte layout was read closely enough
    to catch it).
  - `IH`/`IL` — the SDATA parameter index (see the table below), split
    14-bit: `index = (IH << 7) | IL`. For every index in this project's
    range (0-127ish) `IH` is always `0`.
  - `XX` — the raw parameter byte, same range/meaning as the SDATA
    table's "Range" column.
  - No checksum — `modifySingle()` writes `*p = _data[IdxSingleParamValue]`
    with no checksum check at all, unlike a full dump.
- **Implemented**: `xenia_plugin.cpp`'s `sendSingleParamChange()` +
  `kSysexParams` (currently only the filter section — see that file's own
  comments for exactly which keys and why only those).

### The SDATA parameter index table (partial — get more pages if needed)

Confirmed indices, from the device manual's "3. Data Formats / 3.1 SDATA
— Sound Data" table (**not** the MIDI Implementation Chart — a different
page of the same manual):

| Index | Range | Parameter |
|---|---|---|
| 62 | 0-127 | Filter 1 Cutoff |
| 63 | 0-127 | Filter 1 Resonance |
| 64 | 0-9 | Filter 1 Type |
| 65 | -200%..+197% | Filter 1 Keytrack |
| 66 | -64..+63 | Filter 1 Envelope Amount |
| 67 | 0-127 | Filter 1 Envelope Velocity Amount |
| 73 | 0-127 | Filter 2 Cutoff |
| 74 | 0-1 | Filter 2 Type (6dB LP / 6dB HP only — narrower than Filter 1) |
| 75 | -200%..+197% | Filter 2 Keytrack |
| 76 | 0-7[MW2]/0-35[XT] | Effect Type |
| 77 | 0-127 | Amplifier Volume |
| 79 | -64..+63 | Amplifier Envelope Velocity Amount |
| 81 | 0-127 | Chorus |
| 83 | 0-127 (64=center) | Panning |
| 89 | 0-127 | Glide Time |
| 112-117 | various | Filter Envelope Attack/Decay/Sustain/Release/(reserved)/Trigger |
| 119-122 | various | Amplifier Envelope Attack/Decay/Sustain/(continues on next page) |

**Only the filter-section indices (62-67, 73) are wired up in code so
far.** The rest of `kParams`' ~75 other keys (OSC, MIXER, LFO1/2, ARP,
WAVE MOD, etc.) almost certainly need the exact same treatment — a real
SDATA index looked up and added to `kSysexParams`, not the CC numbers
currently in `kParams`' `outCC` field, which are very likely *all*
equally non-functional for the same reason the filter was. Don't
convert them by guessing indices from the pattern above — get the actual
remaining SDATA table pages and read the real index for each key, the
same way the filter section was done. Guessing wrong writes into some
*other* unintended parameter, silently, which is a worse failure mode
than "no effect."

### Everything already ruled out, so it isn't re-tried

All of the following were tested via `xenia_render` (some also
confirmed audibly on real Push hardware) and produced the **same flat,
no-discernible-change result** — treat this as settled unless something
above turns out to be wrong:

- Plain CC50 (cutoff) swept 0→127, plain CC56 (resonance) swept 0→125.
- Resonance pinned near self-oscillation (115) while cutoff sweeps — a
  real analog-style filter should be dramatically audible here; it
  wasn't, ruling out "just a subtle patch/filter type."
- Filter 2 (CC60) instead of Filter 1, same resonance-heavy setup — rules
  out "wrong filter for this patch."
- All 10 `filter_type` (CC54) values, each with its own internal cutoff
  sweep — rules out "wrong filter type/mode selected."
- Multiple programs (0, 1, 2, 5) with the same sweep inside each — rules
  out "this one patch's mod matrix has nothing routed" as the *only*
  explanation (every patch tested behaved the same).
- **The real SysEx Single Parameter Change** (command 0x20) for cutoff
  and resonance, both with `LOC=0x20` and the corrected `LOC=0x00` — the
  protocol-correct mechanism per the manual, verified byte-for-byte
  against gearmulator's own parsing source, still produced no audible
  change.

### Why deeper C++ instrumentation of `xtState.cpp` won't help further

`xenia_render` (the offline test tool) constructs `xt::Xt` directly and
calls `Xt::sendMidiEvent()`, which goes straight to
`Hardware::sendMidi()` — the **raw UART/68k emulation path**, the same
one real MIDI over a cable would take. It does **not** go through
`xt::Device::sendMidi()` (a higher-level wrapper with its own
`m_state.receive()` native-C++ shortcut/cache that `xenia_plugin.cpp`'s
actual plugin instance uses). This was confirmed by instrumenting
`xt::State::receive()`/`getSingleParameter()`/`modifySingle()` with debug
`fprintf`s, rebuilding, and running the exact same SysEx test — zero
debug output, proving those functions are never even called from
`xenia_render`. **This means `xenia_render`'s results are the most
authentic possible test** (real firmware machine code parses the
message, same as real hardware would) — but it also means there's no
native C++ breakpoint/log point left to add; the actual CC/SysEx
dispatch logic lives entirely in the compiled ROM's 68k machine code,
invisible to any further source-level instrumentation. If this needs
resolving further without new documentation pages, the tool for it is a
68k disassembler/debugger attached to the ROM's SysEx-handling routine,
not more `xtLib` C++ edits.

(Side note while instrumenting: this sandbox has **two separate copies**
of the gearmulator source tree — `~/charlesvestal/gearmulator` (the one
the actual CMake build in `~/xenia-build` references and compiles from)
and a second copy under the scratchpad directory. Editing the scratchpad
copy silently has no effect on the build — always check `build.ninja`'s
recorded source path for the target you're rebuilding before assuming an
edit will take effect, exactly the class of mistake that cost a full
rebuild-and-test round here.)

### Next steps

1. **Test the pushed SysEx fix on real Push hardware** — `xenia_render`'s
   minimal boot sequence (it only waits for the DSP to start producing
   audio, not necessarily every NVRAM/EEPROM init routine a full power-on
   does) might differ from real hardware in some way that matters here;
   real hardware is the actual target and hasn't been tested with this
   specific fix yet.
2. If still silent on real hardware: get the remaining SDATA table pages
   (indices continue past 122) and convert the rest of `kParams` the same
   way — it's very likely *all* of it needs this, not just the filter.
3. If filter specifically still doesn't respond even via confirmed-correct
   SysEx: the per-patch "local MIDI receive" possibility from the
   original investigation is still open — check the manual for a
   per-single-patch MIDI-enable byte distinct from the global
   `ParameterReceive`.
