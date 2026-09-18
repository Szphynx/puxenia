# Xenia / gearmulator (Waldorf Microwave II/XT) notes

Findings specific to the actual synth engine this plugin wraps — gained by
reading `gearmulator`'s source directly
(`charlesvestal/gearmulator/source/waldi/xt/xtLib`) and by offline testing,
not duplicated from that repo's own code comments.

> **Read this first if you're about to send a SysEx message to this
> device.** `gearmulator` ships a real, working reference implementation
> at `source/waldi/xt/xtJucePlugin/` — a full JUCE plugin with a working
> parameter editor. Its packet layouts, in
> `xtJucePlugin/parameterDescriptions_xt.json`'s `"midipackets"` section,
> are literal byte-by-byte templates and are ground truth. Reconstructing
> a SysEx message's byte layout from `xtLib`'s C++ parsing code alone
> (`xtState.cpp`, `xtMidiTypes.h`) is how a real, shipped bug got
> introduced in this project (see "The real answer" section below) — the
> generic index-parsing helper there is shared infrastructure that looks
> like it implies one byte layout when the actual packet template uses a
> different one. Check the JSON template first, always.

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
command. Don't reverse-engineer its byte layout from `xtMidiTypes.h`/
`xtState.cpp` alone (that was tried, got the field count wrong, and
cost real debugging time) — **gearmulator ships a real, working
reference implementation**, a full JUCE plugin with a working editor
GUI, at `source/waldi/xt/xtJucePlugin/`. Its packet layouts are
authoritative, ground truth, data-driven, and trivial to read:

- **Reference source**: `xtJucePlugin/parameterDescriptions_xt.json`,
  the `"midipackets"` object, `"singleparameterchange"` entry — a literal
  byte-by-byte packet template. Cross-check against
  `xtController.cpp`'s `Controller::sendParameterChange()`, which shows
  what value each named field actually gets at send time.
- **Command**: `SysexCommand::SingleParameterChange = 0x20` (`xtMidiTypes.h`)
- **Real wire format** (confirmed from the JSON, not guessed):
  `F0 3E 0E DEV 20h PART PAGE IDX XX F7` — **four single-byte fields**,
  not a "location byte + 2-byte split index" the way `xtState.cpp`'s
  generic `getParameter()` helper alone would suggest (that helper's
  `idxParamIndexH`/`idxParamIndexL` machinery is generic infrastructure
  shared with Multi/Global param changes that use a real 2-byte index;
  Single's own packet just happens to route both of those "index" slots
  to two *different*, separately-meaningful fields, PART and PAGE, with
  the actual parameter index as its own separate single byte after
  them — a distinction not obvious from `xtState.cpp` in isolation).
  - `DEV` — device ID; `0x7F` (`wLib::IdDeviceOmni`) is a real, defined
    broadcast constant, not a guess.
  - `PART` — `0x00` for a Single (non-Multi) parameter. In the reference
    plugin, `Parameter::getPart()` is what fills this, and it's 0 outside
    Multi mode. **This field was the actual bug**: an earlier version of
    this code guessed `0x20` here (misreading it as a "location" byte,
    and even then guessing wrong) — a real, shipped, hardware-tested bug,
    not just a theory; the wrong value silently made every Single
    Parameter Change a no-op.
  - `PAGE` — the parameterdescriptions JSON's own `"page"` field for that
    parameter, defaulting to `0`; confirmed `0` for every filter-section
    entry actually used (e.g. `F1Cutoff`'s JSON entry has no `"page"`
    override).
  - `IDX` — the SDATA table's "Index" column (e.g. `62` for Filter 1
    Cutoff), a single byte — **not** split across two bytes.
  - `XX` — the raw parameter byte, same range/meaning as the SDATA
    table's "Range" column.
  - No checksum — `modifySingle()` writes `*p = _data[IdxSingleParamValue]`
    with no checksum check at all, unlike a full dump.
- **Implemented**: `xenia_plugin.cpp`'s `sendSingleParamChange()` +
  `kSysexParams`, now covering **all 76 of the 82 `kParams` keys** that
  aren't Program Change or one of the 5 confirmed-hardwired CCs. **Not
  yet tested on real hardware** — the one hardware test done so far used
  the earlier, wrong `PART=0x20` build with only 7 keys converted.

### The SDATA parameter index table — use the JSON, not the PDF

Don't re-read PDF pages for this. `gearmulator`'s reference plugin ships
the **complete** table as data:
`xtJucePlugin/parameterDescriptions_xt.json`'s `"parameterdescriptions"`
array — 451 entries, every one with `page`/`index`/`name`/`min`/`max`
(and `default` where it's not the schema default). Single-mode params
(everything `kParams` needs) are `"page":0` (or the field is simply
absent, which means 0 per `"parameterdescriptiondefaults"`). Pages
10-18 are the 8 Multi-mode instrument parts — irrelevant to this
project's single-patch editing. Look a parameter up by name, e.g.:

```bash
python3 -c "
import re, json
with open('source/waldi/xt/xtJucePlugin/parameterDescriptions_xt.json') as f:
    content = re.sub(r'//.*', '', f.read())
params = {p['name']: p for p in json.loads(content)['parameterdescriptions'] if p.get('page',0)==0}
print(params['F1Cutoff'])
"
```

(The file has `//` comments, so strip them before `json.loads` — plain
`json.load` fails on it directly.)

The table also has two things worth knowing about for future feature
work, found while reading it for this: a **real 16-slot modulation
matrix** (`Slot1Source`/`Slot1Amount`/`Slot1Destination` through
`Slot16...`, indices 192-239, each Source 0-31 and Destination 0-35 —
exactly the shape needed for an MPE-to-parameter mod-matrix feature, no
guessing required), and the **patch name as 16 raw SysEx-writable bytes**
(`Name00`-`Name15`, indices 240-255, range 32-127 i.e. printable ASCII) —
readable via a Single Dump request, useful for a real preset-name
display instead of the current "Program NNN" placeholder.

`kParams`' keys were matched to this table by name (e.g. `cutoff` →
`F1Cutoff`) and the mapping is already done — see `kSysexParams` in
`xenia_plugin.cpp`. If a *new* param is ever added to `kParams`, look it
up in this JSON the same way; don't guess an index by pattern-matching
neighboring ones.

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

1. **Test the corrected, complete SysEx conversion on real Push
   hardware** — not yet done as of this writing. The one hardware test
   run so far used the earlier, wrong `PART=0x20` build with only the
   filter section converted; both the field-layout bug and the "only 7 of
   82 params converted" gap are now fixed. This is the most promising
   untested lead by far.
2. If filter specifically still doesn't respond even via the corrected
   SysEx: the per-patch "local MIDI receive" possibility from the
   original investigation is still open — check the manual for a
   per-single-patch MIDI-enable byte distinct from the global
   `ParameterReceive`.
