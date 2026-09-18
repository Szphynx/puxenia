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

## Known open issue (as of this writing)

**Cutoff/resonance CCs (and likely most of the 82-param table beyond
Program Change/Pitch Bend) produce no audible change on real hardware**,
despite: the CC numbers being transcribed correctly from the real chart,
the outbound MIDI byte-level dispatch confirmed correct via added
`fprintf` logging in `xenia_set_param`, and the `ParameterReceive` global
already defaulting to on. Pitch Bend and Program Change *do* work
audibly. Next steps to try, in order of how promising they seem:

1. Run the two sweep scripts above through `xenia_render` (with absolute
   paths!) and actually inspect the resulting WAVs — this hasn't been
   done successfully yet as of this writing (blocked purely on the path
   bug above, now documented).
2. If the sweep confirms silence: investigate whether cutoff/resonance
   route through the `ControllerW/X/Y/Z` generic-assignable-controller
   mechanism rather than being hardwired, and whether the currently
   loaded factory patch's own modulation matrix has anything routed from
   those slots at all.
3. If they're genuinely hardwired (not mod-matrix-dependent) per the real
   chart, the remaining candidates are: `BendRange`/`DeviceId`/
   `MidiChannel` global mismatches (already checked — Program Change
   works on the exact same channel these CCs are sent on, so a channel
   mismatch would break both, not just CCs), or a per-patch "local MIDI
   receive" flag stored in the patch data itself (distinct from the
   global `ParameterReceive`) that some patches disable — check the real
   Microwave XT manual/SysEx spec for a per-single-patch MIDI-enable byte
   in the patch dump format, not just the global parameter table.
