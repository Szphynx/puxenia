/*
 * xenia_plugin.cpp — Waldorf Microwave II/XT (Xenia) emulation as a
 * Move Anything plugin_api_v2 module, for push-hack-braids/push-hack-xenia's
 * generic host (src/bridge.c dlopen's any .so exporting
 * move_plugin_init_v2 -- nothing about that host is Braids/Xenia-specific).
 *
 * Phase 2 (confirmed working on real Push 3 hardware -- audible pad
 * presses recorded on an isolated MIDI track, 302 notes / 0 xrun retries
 * over a 4.5 minute run) is the baseline this file builds on. Phase 3
 * additions in this revision:
 *
 *   - Proactive lookahead buffering in render_block (kLookaheadFrames):
 *     the hardware run showed 22 "slow block" warnings -- each one a
 *     render_block call that had to burst 2-3 native blocks of DSP work
 *     (voice alloc / filter recalc on a fresh note landing right when the
 *     pending buffer was nearly empty) inside one fixed host callback
 *     budget. No xruns resulted (there was slack in the ALSA period), but
 *     it's the one real headroom risk from that run. Fix: always keep a
 *     few blocks of margin buffered instead of only refilling exactly
 *     enough for the current call, so a heavy block's extra cost gets
 *     spread across calls rather than bursting into just the one call
 *     unlucky enough to coincide with it.
 *   - A safety layer: a fixed output headroom ceiling (this is a
 *     hardware bring-up bridge, not a mixed/mastered signal chain -- a
 *     stuck note or unexpected resonant self-oscillation should not be
 *     able to hit full scale into someone's monitors), a stuck-note
 *     watchdog (forces a note-off if held past kMaxNoteHoldSeconds --
 *     guards against a dropped MIDI message, e.g. Push losing connection
 *     mid-hold, leaving a voice stuck on forever), and a try/catch around
 *     the calls the host invokes every block/instance/message so a
 *     construction or rendering fault can't take the whole host process
 *     down.
 *   - All 82 live parameters (kParams below) are exposed via
 *     chain_params/get_param/set_param so the Go host's own on-screen
 *     encoder UI and web UI can drive them. NOT reachable via raw
 *     incoming MIDI CC: push-xenia's main.go only ever forwards Note
 *     On/Off to on_midi -- every CC (including Push's own encoders) is
 *     consumed entirely inside the Go host for its own UI and turned
 *     into set_param calls instead, which then send the corresponding
 *     outbound CC (per kParams' outCC) to the device -- that direction
 *     has nothing to do with what on_midi receives.
 */

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>
#include <map>
#include <algorithm>
#include <exception>
#include <unistd.h>

#include "synthLib/midiTypes.h"
#include "xtLib/xt.h"
#include "xtLib/xtRomLoader.h"
#include "dsp56kBase/logging.h"
#include "cpu/mc68k/logging.h"
#include "libresample.h"

// mc68k::logToConsole (MCLOG, used by xtPic.cpp for every LCD content
// change -- boot animation, preset-name scrolling) has no Logging::setLogFunc-
// style hook like dsp56kBase's does; its implementation is a hardcoded
// fputs(..., stderr) with no override point. It used to be treated as
// harmless noise, but on real hardware the SLOW BLOCK warnings during boot
// and preset changes line up exactly with LCD text scrolling: each MCLOG
// call is a synchronous, unbuffered stderr write on the real-time render
// thread, and over an SSH pipe (not a local tty) that write can block long
// enough to blow the ~2.7ms block budget by 2-4x -- this is what "the
// interface goes incredibly slow when changing presets" and part of the
// reported audio glitching actually was. mc68k::logToConsole is a plain
// (non-weak) symbol in a static archive (libwLib.a); providing our own
// definition here, in an object file linked directly rather than pulled
// from the archive, makes the linker resolve the symbol to this one and
// never pull in the archive's version -- the standard static-library
// override trick, not a source patch to gearmulator itself.
// Captured LCD content, parsed out of the very same MCLOG calls the
// override above used to just discard. This is the ONLY source of real
// patch names/bank letters this host has -- Xenia has no MIDI name
// readback, but the emulated LCD prints exactly what a real Microwave
// XT's screen would show, e.g.:
//   "  Play Sound A099  |  Mode   |Main Vol."
//   "  MonasteryChoirGM |  Sound  |   127"
// Line 1's last space-separated token is bank+number ("A099"); line 2's
// first '|'-delimited field is the patch name. Only valid for Single mode's
// default Play screen -- Multi mode or a different screen would need a
// different parse, not attempted here. Single-threaded by construction:
// MCLOG only ever fires from inside device->process(), called only from
// fillMoreOutput on the one goroutine that also calls get_param (see
// main.go's midiHandler doc comment) -- no lock needed.
std::string g_lcdLine1, g_lcdLine2;

namespace mc68k
{
	void logToConsole(const std::string &s)
	{
		auto pos = s.find("LCD:\n");
		if(pos == std::string::npos)
			return;
		std::string content = s.substr(pos + 5);
		auto nl = content.find('\n');
		if(nl == std::string::npos)
		{
			g_lcdLine1 = content;
			g_lcdLine2.clear();
		}
		else
		{
			g_lcdLine1 = content.substr(0, nl);
			g_lcdLine2 = content.substr(nl + 1);
		}
	}
}

namespace
{
	std::string trimmed(const std::string &s)
	{
		const auto a = s.find_first_not_of(" \t");
		if(a == std::string::npos)
			return "";
		const auto b = s.find_last_not_of(" \t");
		return s.substr(a, b - a + 1);
	}

	std::string firstField(const std::string &line)
	{
		const auto pos = line.find('|');
		return trimmed(pos == std::string::npos ? line : line.substr(0, pos));
	}

	std::string lastToken(const std::string &s)
	{
		const auto t = trimmed(s);
		const auto pos = t.find_last_of(' ');
		return pos == std::string::npos ? t : t.substr(pos + 1);
	}

	constexpr uint32_t kNativeBlock = 64;
	constexpr double kNativeRate = 40000.0; // xtLib/xtDevice.cpp: Device::getSamplerate()

	// Empirically confirmed by bisection against real hardware (see
	// xenia_render's sweep_ch*.txt tests): this ROM's default boot state
	// only answers on MIDI channel 10 (0-indexed 9). Real pad MIDI from
	// Push 3 arrives on whatever channel push-manager/braids' own
	// convention uses (channel 1 historically) -- remap here so a
	// human pressing pads doesn't need to know this quirk exists.
	constexpr uint8_t kWorkingChannel0Indexed = 9;

	// Stay this many native blocks ahead of what render_block strictly
	// needs -- see the file header comment on the 22 slow-block warnings
	// this is fixing. 3 blocks * 64 frames = 192 native frames of margin
	// (well under a millisecond of extra latency at 40kHz native rate).
	constexpr int kLookaheadFrames = kNativeBlock * 3;

	// Safety ceiling: -6dBFS. Not a look-ahead limiter, just a fixed
	// headroom cut so nothing unexpected from this bridge (stuck voices,
	// self-oscillating resonance while param mapping is still being
	// tuned) can reach full scale.
	constexpr float kOutputHeadroom = 0.5f;

	// Force a note off if it's been held this long without a matching
	// note-off ever arriving. Guards against a dropped MIDI message (e.g.
	// Push losing its connection mid-hold) leaving a voice stuck forever.
	constexpr double kMaxNoteHoldSeconds = 60.0;

	uint32_t g_hostSampleRate = 44100;

	struct PerChannelResampler
	{
		void *handle = nullptr;
		std::vector<float> pending;  // resampled output not yet consumed by render_block
		std::vector<float> leftover; // input samples libresample didn't consume last call

		void open(double factor)
		{
			handle = resample_open(1 /* high quality */, factor, factor);
		}
		// Re-opens against a corrected factor and drops whatever was
		// pending under the old (wrong) one -- used when the real host
		// sample rate becomes known after load time (see
		// "_host_sample_rate" in xenia_set_param).
		void reopen(double factor)
		{
			if(handle) resample_close(handle);
			handle = resample_open(1, factor, factor);
			pending.clear();
			leftover.clear();
		}
		~PerChannelResampler()
		{
			if(handle) resample_close(handle);
		}
	};

	struct ActiveNote
	{
		uint8_t note = 0;
		double heldSeconds = 0.0;
	};

	// One entry per exposed param. outCC is the MIDI CC this param is
	// forwarded to the device as. Verified against the real Waldorf
	// Microwave II/XT/XTk "MIDI Controller Assignments" chart (software
	// release 2.28, from the user's manual) -- every CC number below is
	// transcribed directly from that chart, not guessed. "program" is the
	// sole exception: it's a Program Change, not a CC (kProgramSentinel).
	// Ranges follow the chart's "Contr. No. Range" column (most are
	// 0-127; a few, like Osc Octave or LFO Shape, are narrower enums the
	// device itself only reads the low end of).
	struct ParamDef
	{
		const char *key;
		const char *name;
		int minV;
		int maxV;
		uint8_t outCC;
		uint8_t defaultV;
	};
	constexpr uint8_t kProgramSentinel = 0xFF; // "key is a Program Change, not a CC"
	constexpr ParamDef kParams[] = {
		// -- FILTER page --
		{"program",            "Program",             0, 127, kProgramSentinel, 0},
		// Bank Select (CC32) -- confirmed hardwired per the real MIDI
		// Implementation Chart (the same chart that confirmed the 7
		// directly-recognized CCs elsewhere in this file), sent before a
		// Program Change to pick which bank of 128 the change addresses.
		// Range 0-7 is a placeholder, not verified against real hardware
		// -- the emulated LCD's bank letter (see lcd_patch_id below) is
		// the way to find out how many banks this ROM/card setup actually
		// has: step this and watch whether the letter changes or the
		// device just ignores out-of-range values.
		{"bank",                "Bank",                0, 7,   32, 0},
		{"cutoff",              "Filter 1 Cutoff",     0, 127, 50, 100},
		{"resonance",           "Filter 1 Resonance",  0, 127, 56, 0},
		{"filter_type",         "Filter 1 Type",       0, 12,  54, 0},
		{"filter_keytrack",     "Filter 1 Keytrack",   0, 127, 51, 64},
		{"filter_env_amount",   "Filter 1 Env Amount", 0, 127, 52, 64},
		{"filter_env_velocity", "Filter 1 Env Velo",   0, 127, 53, 64},
		{"filter2_cutoff",      "Filter 2 Cutoff",     0, 127, 60, 100},
		// -- OSC page --
		{"osc1_octave",   "Osc 1 Octave",   16, 112, 33, 64},
		{"osc1_semitone", "Osc 1 Semitone", 52, 76,  34, 64},
		{"osc1_detune",   "Osc 1 Detune",   0,  127, 35, 64},
		{"osc1_wave",     "Wave 1 Start",   0,  63,  71, 0},
		{"osc2_octave",   "Osc 2 Octave",   16, 112, 38, 64},
		{"osc2_semitone", "Osc 2 Semitone", 52, 76,  39, 64},
		{"osc2_detune",   "Osc 2 Detune",   0,  127, 40, 64},
		{"osc2_wave",     "Wave 2 Start",   0,  63,  77, 0},
		// -- MIXER page --
		{"wave1_level",  "Wave 1 Level", 0, 127, 45, 100},
		{"wave2_level",  "Wave 2 Level", 0, 127, 46, 0},
		{"ringmod_level","Ring Mod Lvl", 0, 127, 47, 0},
		{"noise_level",  "Noise Level",  0, 127, 48, 0},
		{"osc2_sync",    "Osc 2 Sync",   0, 1,   41, 0},
		{"osc2_link",    "Osc 2 Link",   0, 1,   44, 0},
		{"fm_amount",    "FM Amount",    0, 127, 13, 0},
		{"wavetable",    "Wavetable",    0, 127, 70, 0},
		// -- FILTER ENV page --
		{"f_attack",  "Filter Env Attack",  0, 127, 14, 0},
		{"f_decay",   "Filter Env Decay",   0, 127, 15, 64},
		{"f_sustain", "Filter Env Sustain", 0, 127, 16, 100},
		{"f_release", "Filter Env Release", 0, 127, 17, 0},
		{"f_trigger", "Filter Env Trigger", 0, 2,   29, 0},
		// -- AMP page --
		{"a_attack",     "Amp Env Attack",  0, 127, 18, 0},
		{"a_decay",      "Amp Env Decay",   0, 127, 19, 64},
		{"a_sustain",    "Amp Env Sustain", 0, 127, 20, 100},
		{"a_release",    "Amp Env Release", 0, 127, 21, 32},
		{"a_velocity",   "Amp Env Velo",    0, 127, 58, 64},
		{"a_trigger",    "Amp Env Trigger", 0, 2,   31, 0},
		{"amp_volume",   "Amp Volume",      0, 127, 57, 100},
		{"amp_keytrack", "Amp Keytrack",    0, 127, 55, 64},
		// -- LFO 1 page --
		{"lfo1_rate",     "LFO 1 Rate",     0, 127, 24,  64},
		{"lfo1_shape",    "LFO 1 Shape",    0, 5,   25,  0},
		{"lfo1_delay",    "LFO 1 Delay",    0, 127, 30,  0},
		{"lfo1_sync",     "LFO 1 Sync",     0, 3,   112, 0},
		{"lfo1_symmetry", "LFO 1 Symmetry", 0, 127, 113, 64},
		{"lfo1_humanize", "LFO 1 Humanize", 0, 127, 114, 0},
		// -- LFO 2 page --
		{"lfo2_rate",     "LFO 2 Rate",     0, 127, 26,  64},
		{"lfo2_shape",    "LFO 2 Shape",    0, 5,   28,  0},
		{"lfo2_delay",    "LFO 2 Delay",    0, 127, 27,  0},
		{"lfo2_sync",     "LFO 2 Sync",     0, 3,   115, 0},
		{"lfo2_symmetry", "LFO 2 Symmetry", 0, 127, 116, 64},
		{"lfo2_humanize", "LFO 2 Humanize", 0, 127, 117, 0},
		{"lfo2_phase",    "LFO 2 Phase",    0, 127, 118, 0},
		// -- GLOBAL page --
		{"mod_wheel",      "Mod Wheel",      0, 127, 1,  0},
		{"channel_volume", "Channel Volume", 0, 127, 7,  100},
		{"panning",        "Panning",        0, 127, 10, 64},
		{"chorus",         "Chorus",         0, 1,   12, 0},
		{"sustain",        "Sustain",        0, 127, 64, 0},
		{"glide_time",     "Glide Time",     0, 127, 5,  0},
		{"glide_type",     "Glide Type",     0, 3,   22, 0},
		{"glide_mode",     "Glide Mode",     0, 1,   23, 0},
		// -- FREE ENV page --
		{"fe_time1",    "Free Env Time 1",    0, 127, 85, 0},
		{"fe_level1",   "Free Env Level 1",   0, 127, 86, 64},
		{"fe_time2",    "Free Env Time 2",    0, 127, 87, 0},
		{"fe_level2",   "Free Env Level 2",   0, 127, 88, 64},
		{"fe_time3",    "Free Env Time 3",    0, 127, 89, 0},
		{"fe_level3",   "Free Env Level 3",   0, 127, 90, 64},
		{"fe_rel_time",  "Free Env Rel Time",  0, 127, 91, 0},
		{"fe_rel_level", "Free Env Rel Level", 0, 127, 92, 64},
		// -- ARP page --
		{"arp_active",        "Arp Active",   0, 2,   102, 0},
		{"arp_range",         "Arp Range",    0, 9,   103, 0},
		{"arp_clock",         "Arp Clock",    0, 15,  104, 0},
		{"arp_tempo",         "Arp Tempo",    0, 127, 105, 0},
		{"arp_direction",     "Arp Direction",0, 3,   106, 0},
		{"arp_pattern",       "Arp Pattern",  0, 16,  107, 0},
		{"arp_note_order",    "Arp Note Ord", 0, 3,   108, 0},
		{"arp_pattern_length","Arp Pat Len",  0, 15,  111, 0},
		// -- WAVE MOD page --
		{"wave1_phase",     "Wave 1 Phase",    0, 127, 72, 0},
		{"wave1_env_amt",   "Wave 1 Env Amt",  0, 127, 73, 64},
		{"wave1_env_vel",   "Wave 1 Env Velo", 0, 127, 74, 64},
		{"wave1_keytrack",  "Wave 1 Keytrack", 0, 127, 75, 64},
		{"wave2_phase",     "Wave 2 Phase",    0, 127, 78, 0},
		{"wave2_env_amt",   "Wave 2 Env Amt",  0, 127, 79, 64},
		{"wave2_env_vel",   "Wave 2 Env Velo", 0, 127, 80, 64},
		{"wave2_keytrack",  "Wave 2 Keytrack", 0, 127, 81, 64},
	};
	constexpr size_t kNumParams = sizeof(kParams) / sizeof(kParams[0]);

	const ParamDef *findParam(const char *key)
	{
		for(const auto &p : kParams)
			if(strcmp(p.key, key) == 0)
				return &p;
		return nullptr;
	}

	struct XeniaInstance
	{
		xt::Xt *device = nullptr;
		bool bootFailed = false;
		std::string lastError;

		PerChannelResampler resampL;
		PerChannelResampler resampR;
		double resampleFactor = 1.0;

		// Native-rate scratch buffers, refilled kNativeBlock frames at a
		// time and drained through the resampler into resampL/R.pending.
		std::vector<float> nativeL;
		std::vector<float> nativeR;

		// One live value per kParams entry, keyed by ParamDef::key.
		// Populated with each ParamDef's defaultV in xenia_create_instance.
		std::map<std::string, uint8_t> paramValues;

		// Raw bytes read back from the device's own MIDI OUT since the last
		// complete SysEx frame was consumed -- see drainDeviceMidiOut. A
		// Single Dump response can take many fillMoreOutput calls' worth of
		// (paced, realistic-baud-rate) device-time to fully arrive, so this
		// has to persist across calls rather than being a local variable.
		std::vector<uint8_t> midiOutAccum;

		std::vector<ActiveNote> activeNotes;

		~XeniaInstance()
		{
			delete device;
		}
	};

	int32_t signExtend24(dsp56k::TWord w)
	{
		struct { int32_t v : 24; } s{ static_cast<int32_t>(w & 0xFFFFFF) };
		return s.v;
	}

	void sendToDevice(XeniaInstance *inst, uint8_t status, uint8_t d1, uint8_t d2)
	{
		synthLib::SMidiEvent ev(synthLib::MidiEventSource::Host, status, d1, d2);
		inst->device->sendMidiEvent(ev);
	}

	// sendGlobalParamChange sends a GLBP (Global Parameter Change, IDM
	// 0x24h per the Microwave 2 System Exclusive spec) SysEx -- format
	// F0 3E 0E DEV 24h PP XX F7, no checksum (the spec notes parameter-
	// change messages omit it). DEV=0x7F is the broadcast device number,
	// always accepted regardless of the receiving unit's own configured
	// "Device ID" global parameter -- safer than guessing DEV=0x00 and
	// being silently ignored if it's ever been changed from factory
	// default.
	void sendGlobalParamChange(XeniaInstance *inst, uint8_t paramIndex, uint8_t value)
	{
		synthLib::SMidiEvent ev(synthLib::MidiEventSource::Host);
		auto &sx = ev.sysex;
		sx.push_back(0xF0);
		sx.push_back(0x3E); // Waldorf Electronics GmbH ID
		sx.push_back(0x0E); // Microwave 2 ID
		sx.push_back(0x7F); // DEV: broadcast
		sx.push_back(0x24); // IDM: GLBP
		sx.push_back(paramIndex);
		sx.push_back(value);
		sx.push_back(0xF7);
		inst->device->sendMidiEvent(ev);
	}

	// sendSingleParamChange sends a Sound/Single Parameter Change SysEx
	// (IDM 0x20h) -- format F0 3E 0E DEV 20h PART PAGE IDX XX F7. This,
	// not a plain Control Change, is how most sound-editing parameters
	// (filter cutoff/resonance/etc.) are actually meant to be set in real
	// time: the device's own MIDI Implementation Chart lists only 7 CCs
	// as directly recognized (1/2/5/7/10/32/64 -- Modwheel/Breath/
	// Portamento Time/Volume/Pan/Bank Select/Sustain), with a footnote
	// pointing elsewhere for everything else. That "everything else"
	// turned out to be this SysEx mechanism, addressed by the SDATA
	// byte-offset table.
	//
	// This exact field layout (PART, PAGE, IDX, XX -- four single-byte
	// fields, not the two-byte 14-bit-split index an earlier version of
	// this function guessed from xtState.cpp's generic getParameter()
	// helper alone) is confirmed straight from gearmulator's own
	// reference JUCE plugin, which is a real, working implementation:
	// xtJucePlugin/parameterDescriptions_xt.json's "midipackets" ->
	// "singleparameterchange" entry, cross-checked against
	// xtController.cpp's sendParameterChange(). PART is 0 for a Single
	// (non-Multi) parameter (Controller::sendParameterChange's default
	// case always inserts Parameter::getPart(), which is 0 outside Multi
	// mode). PAGE is the parameterdescriptions JSON's own "page" field,
	// which defaults to 0 and is 0 for every filter-section entry.
	void sendSingleParamChange(XeniaInstance *inst, uint8_t paramIndex, uint8_t value)
	{
		synthLib::SMidiEvent ev(synthLib::MidiEventSource::Host);
		auto &sx = ev.sysex;
		sx.push_back(0xF0);
		sx.push_back(0x3E);
		sx.push_back(0x0E);
		sx.push_back(0x7F);  // DEV: broadcast
		sx.push_back(0x20);  // IDM: Single Parameter Change
		sx.push_back(0x00);  // PART: 0 (not in Multi mode)
		sx.push_back(0x00);  // PAGE: 0 (every filter-section param's real page)
		sx.push_back(paramIndex);
		sx.push_back(value);
		sx.push_back(0xF7);
		inst->device->sendMidiEvent(ev);
	}

	// kSysexParams: every kParams key that needs sendSingleParamChange
	// instead of a plain CC -- see sendSingleParamChange's doc above for
	// why, and docs/xenia-gearmulator-notes.md for how these indices were
	// obtained (gearmulator's own reference JUCE plugin's
	// parameterDescriptions_xt.json, not the PDF chart -- ground truth,
	// covers every single-mode parameter). The only kParams keys
	// deliberately NOT here are the 5 confirmed-hardwired CCs per the
	// real MIDI Implementation Chart -- mod_wheel(CC1), channel_volume
	// (CC7), panning(CC10), sustain(CC64), glide_time(CC5) -- which stay
	// on the plain-CC path because they already work.
	struct SysexParamDef { const char *key; uint8_t sdataIndex; };
	constexpr SysexParamDef kSysexParams[] = {
		{"cutoff", 62}, {"resonance", 63}, {"filter_type", 64}, {"filter_keytrack", 65},
		{"filter_env_amount", 66}, {"filter_env_velocity", 67}, {"filter2_cutoff", 73},
		{"osc1_octave", 1}, {"osc1_semitone", 2}, {"osc1_detune", 3}, {"osc1_wave", 26},
		{"osc2_octave", 12}, {"osc2_semitone", 13}, {"osc2_detune", 14}, {"osc2_wave", 36},
		{"wave1_level", 47}, {"wave2_level", 48}, {"ringmod_level", 49}, {"noise_level", 50},
		{"osc2_sync", 16}, {"osc2_link", 19}, {"fm_amount", 7}, {"wavetable", 25},
		{"f_attack", 113}, {"f_decay", 114}, {"f_sustain", 115}, {"f_release", 116}, {"f_trigger", 117},
		{"a_attack", 119}, {"a_decay", 120}, {"a_sustain", 121}, {"a_release", 122}, {"a_trigger", 123},
		{"a_velocity", 79}, {"amp_volume", 77}, {"amp_keytrack", 80},
		{"lfo1_rate", 159}, {"lfo1_shape", 160}, {"lfo1_delay", 161}, {"lfo1_sync", 162},
		{"lfo1_symmetry", 163}, {"lfo1_humanize", 164},
		{"lfo2_rate", 166}, {"lfo2_shape", 167}, {"lfo2_delay", 168}, {"lfo2_sync", 169},
		{"lfo2_symmetry", 170}, {"lfo2_humanize", 171}, {"lfo2_phase", 172},
		{"chorus", 82}, {"glide_type", 88}, {"glide_mode", 89},
		{"fe_time1", 149}, {"fe_level1", 150}, {"fe_time2", 151}, {"fe_level2", 152},
		{"fe_time3", 153}, {"fe_level3", 154}, {"fe_rel_time", 155}, {"fe_rel_level", 156},
		{"arp_active", 92}, {"arp_range", 95}, {"arp_clock", 94}, {"arp_tempo", 93},
		{"arp_direction", 97}, {"arp_pattern", 96}, {"arp_note_order", 98}, {"arp_pattern_length", 101},
		{"wave1_phase", 27}, {"wave1_env_amt", 28}, {"wave1_env_vel", 29}, {"wave1_keytrack", 30},
		{"wave2_phase", 37}, {"wave2_env_amt", 38}, {"wave2_env_vel", 39}, {"wave2_keytrack", 40},
	};
	const SysexParamDef *findSysexParam(const char *key)
	{
		for(const auto &p : kSysexParams)
			if(strcmp(p.key, key) == 0)
				return &p;
		return nullptr;
	}

	// sendEditBufferRequest asks the device to dump its current Edit
	// Buffer -- the patch actually driving the audio engine right now, as
	// opposed to any specific bank/program slot -- via a Single Request
	// SysEx (IDM 0x00), format F0 3E 0E DEV 00 BANK PROGRAM F7 (confirmed
	// from the reference JUCE plugin's parameterDescriptions_xt.json
	// "requestsingle" packet template, same source that fixed
	// sendSingleParamChange's field layout -- see docs/xenia-gearmulator-
	// notes.md). BANK=0x20 is LocationH::SingleEditBufferSingleMode
	// (xtMidiTypes.h) -- confirmed as the real edit-buffer request from
	// xtController.cpp's own requestSingle(SingleEditBufferSingleMode, 0)
	// call sites, not guessed. The device answers with a Single Dump
	// (IDM 0x10, 265 bytes) on its own MIDI OUT, picked up by
	// drainDeviceMidiOut/parseSingleDump.
	void sendEditBufferRequest(XeniaInstance *inst)
	{
		synthLib::SMidiEvent ev(synthLib::MidiEventSource::Host);
		auto &sx = ev.sysex;
		sx.push_back(0xF0);
		sx.push_back(0x3E);
		sx.push_back(0x0E);
		sx.push_back(0x7F); // DEV: broadcast
		sx.push_back(0x00); // IDM: Single Request
		sx.push_back(0x20); // BANK: Edit Buffer, Single mode
		sx.push_back(0x00); // PROGRAM/location: unused for the edit buffer
		sx.push_back(0xF7);
		inst->device->sendMidiEvent(ev);
	}

	// parseSingleDump copies every kSysexParams-covered param's real value
	// out of a complete Single Dump SysEx frame (header F0 3E 0E DEV 10,
	// bank+program at offsets 5-6, then 256 raw SDATA bytes starting at
	// offset 7, then a checksum byte and the closing F7 -- Dumps[Single] in
	// gearmulator's own xtState.h: firstParamIndex=IdxSingleParamFirst=7,
	// dumpSize=265) into inst->paramValues, so get_param("state") (and thus
	// params.go's syncFromPluginState) reports what the device actually
	// just loaded instead of whatever this host last wrote. NOT the 5
	// hardwired-CC params (mod_wheel/channel_volume/panning/sustain/
	// glide_time, see kSysexParams' own doc) -- those aren't in the SDATA
	// table this dump exposes and keep whatever this host last set.
	void parseSingleDump(XeniaInstance *inst, const std::vector<uint8_t> &frame)
	{
		constexpr size_t kHeaderBytes = 7; // F0 3E 0E DEV IDM BANK PROGRAM
		constexpr size_t kFooterBytes = 2; // checksum, F7
		if(frame.size() < kHeaderBytes + kFooterBytes)
			return;
		const size_t paramBytes = frame.size() - kHeaderBytes - kFooterBytes;
		for(const auto &sp : kSysexParams)
		{
			if(sp.sdataIndex >= paramBytes)
				continue;
			inst->paramValues[sp.key] = frame[kHeaderBytes + sp.sdataIndex];
		}
	}

	// drainDeviceMidiOut pulls whatever new bytes the device has put on its
	// own MIDI OUT since the last call (Xt::receiveMidi swaps out and
	// clears the emulator's internal accumulation buffer -- see xt.cpp),
	// reassembles complete SysEx frames across calls (a real dump is paced
	// at the emulator's modeled baud rate -- see hardwareLib/sciMidi.cpp --
	// so it can take many native blocks' worth of device-time to fully
	// arrive, never all in one call), and hands each complete frame to
	// parseSingleDump if it's a Single Dump. Any non-SysEx bytes (this
	// device doesn't send unsolicited note/CC echo back, but be
	// defensive) are just discarded -- this plugin has no use for them.
	// Called every native block from fillMoreOutput, same cadence as
	// process() itself, so this never lets the emulator's own buffer grow
	// unbounded even when no dump was ever requested (the common case:
	// receiveMidi returns empty, the whole function is a couple of cheap
	// no-op checks).
	void drainDeviceMidiOut(XeniaInstance *inst)
	{
		std::vector<uint8_t> fresh;
		inst->device->receiveMidi(fresh);
		if(fresh.empty())
			return;
		inst->midiOutAccum.insert(inst->midiOutAccum.end(), fresh.begin(), fresh.end());

		size_t i = 0;
		while(i < inst->midiOutAccum.size())
		{
			if(inst->midiOutAccum[i] != 0xF0)
			{
				++i;
				continue;
			}
			size_t end = i + 1;
			while(end < inst->midiOutAccum.size() && inst->midiOutAccum[end] != 0xF7)
				++end;
			if(end >= inst->midiOutAccum.size())
			{
				// Frame not fully arrived yet -- drop any junk bytes before
				// it and wait for more on a later call.
				inst->midiOutAccum.erase(inst->midiOutAccum.begin(), inst->midiOutAccum.begin() + static_cast<long>(i));
				return;
			}
			// [i, end] inclusive of the closing F7 is one complete frame.
			if(end - i + 1 > 4 && inst->midiOutAccum[i + 1] == 0x3E &&
				inst->midiOutAccum[i + 2] == 0x0E && inst->midiOutAccum[i + 4] == 0x10)
			{
				std::vector<uint8_t> frame(inst->midiOutAccum.begin() + static_cast<long>(i),
					inst->midiOutAccum.begin() + static_cast<long>(end) + 1);
				parseSingleDump(inst, frame);
			}
			i = end + 1;
		}
		inst->midiOutAccum.clear();
	}

	void trackNoteOn(XeniaInstance *inst, uint8_t note)
	{
		for(auto &n : inst->activeNotes)
		{
			if(n.note == note) { n.heldSeconds = 0.0; return; }
		}
		inst->activeNotes.push_back({note, 0.0});
	}

	void trackNoteOff(XeniaInstance *inst, uint8_t note)
	{
		auto &v = inst->activeNotes;
		v.erase(std::remove_if(v.begin(), v.end(),
			[&](const ActiveNote &n) { return n.note == note; }), v.end());
	}

	// Called once per render_block with the frame count just rendered --
	// ages every held note by that many seconds and force-releases any
	// that have overstayed kMaxNoteHoldSeconds.
	void tickNoteWatchdog(XeniaInstance *inst, int frames)
	{
		if(inst->activeNotes.empty() || g_hostSampleRate == 0)
			return;

		const double elapsed = static_cast<double>(frames) / static_cast<double>(g_hostSampleRate);
		for(auto it = inst->activeNotes.begin(); it != inst->activeNotes.end(); )
		{
			it->heldSeconds += elapsed;
			if(it->heldSeconds >= kMaxNoteHoldSeconds)
			{
				sendToDevice(inst, static_cast<uint8_t>(0x80 | kWorkingChannel0Indexed), it->note, 0);
				it = inst->activeNotes.erase(it);
			}
			else
			{
				++it;
			}
		}
	}

	// Pull kNativeBlock frames from the DSP and push them, resampled,
	// into pending. Called from render_block as many times as needed to
	// satisfy whatever frame count the host asked for, plus the lookahead
	// margin kept beyond that.
	void fillMoreOutput(XeniaInstance *inst)
	{
		inst->device->process(kNativeBlock);
		drainDeviceMidiOut(inst);
		auto &outs = inst->device->getAudioOutputs();

		inst->nativeL.resize(kNativeBlock);
		inst->nativeR.resize(kNativeBlock);
		for(uint32_t i = 0; i < kNativeBlock; ++i)
		{
			// Full-scale of a 24-bit sample is 2^23; normalize to
			// [-1, 1] for libresample's float interface.
			inst->nativeL[i] = static_cast<float>(signExtend24(outs[0][i])) / 8388608.0f;
			inst->nativeR[i] = static_cast<float>(signExtend24(outs[1][i])) / 8388608.0f;
		}

		auto resampleOne = [&](PerChannelResampler &r, std::vector<float> &in)
		{
			// libresample does not promise to consume its entire input
			// buffer in one resample_process call -- inBufferUsed can be
			// less than what was passed in. Silently feeding a fresh
			// kNativeBlock every call (as this used to do) discarded
			// whatever was left unconsumed each time: a small, regular
			// sample loss every block, which is exactly what "chopped up,
			// garbled, digital interference" sounds like. Carry the
			// unconsumed remainder forward instead, same as gearmulator's
			// own synthLib::Resampler::processResample.
			r.leftover.insert(r.leftover.end(), in.begin(), in.end());

			float outBuf[kNativeBlock * 8]; // generous headroom for any upsample factor this project will hit
			int inUsed = 0;
			const int outN = resample_process(r.handle, inst->resampleFactor,
				r.leftover.data(), static_cast<int>(r.leftover.size()), 0,
				&inUsed, outBuf, static_cast<int>(std::size(outBuf)));
			if(outN > 0)
				r.pending.insert(r.pending.end(), outBuf, outBuf + outN);

			if(inUsed > 0)
				r.leftover.erase(r.leftover.begin(), r.leftover.begin() + inUsed);
		};
		resampleOne(inst->resampL, inst->nativeL);
		resampleOne(inst->resampR, inst->nativeR);
	}
}

/* ---- plugin_api_v2 contract -- copied verbatim from
 * push-hack-braids/src/bridge.c so the layout matches byte-for-byte.
 * That file is the authority; if it ever changes, this must change with
 * it, not the other way around. ---- */

extern "C"
{
	typedef struct host_api_v1
	{
		uint32_t api_version;
		int sample_rate;
		int frames_per_block;
		uint8_t *mapped_memory;
		int audio_out_offset;
		int audio_in_offset;
		void (*log)(const char *msg);
		int (*midi_send_internal)(const uint8_t *msg, int len);
		int (*midi_send_external)(const uint8_t *msg, int len);
	} host_api_v1_t;

	typedef struct plugin_api_v2
	{
		uint32_t api_version;
		void *(*create_instance)(const char *module_dir, const char *json_defaults);
		void (*destroy_instance)(void *instance);
		void (*on_midi)(void *instance, const uint8_t *msg, int len, int source);
		void (*set_param)(void *instance, const char *key, const char *val);
		int (*get_param)(void *instance, const char *key, char *buf, int buf_len);
		int (*get_error)(void *instance, char *buf, int buf_len);
		void (*render_block)(void *instance, int16_t *out_interleaved_lr, int frames);
	} plugin_api_v2_t;
}

namespace
{
	void *xenia_create_instance(const char *module_dir, const char * /*json_defaults*/)
	{
		auto *inst = new XeniaInstance();

		// Suppress dsp56kBase's unconditional LOG() -- see xenia_render.cpp
		// for why this exists (it prints on every JIT block/register
		// write and was contaminating every timing measurement before
		// this fix). Scoped to this one hook, safe to call from inside a
		// shared library. mc68k's separate MCLOG has no equivalent hook
		// (see xenia_render.cpp's own comment on this) and writes
		// straight to stderr with no override -- unlike xenia_render,
		// this is a loaded plugin inside a larger host process, so
		// freopen()'ing the whole process's stderr is NOT safe here and
		// is deliberately not done. mc68k's log lines will still appear
		// on the host's stderr; that's a known, accepted noise source,
		// not a bug to chase.
		static bool logSuppressed = false;
		if(!logSuppressed)
		{
			Logging::setLogFunc([](const std::string &) {});
			logSuppressed = true;
		}

		try
		{
			std::string romDir = std::string(module_dir) + "/roms";
			if(chdir(romDir.c_str()) != 0)
			{
				inst->lastError = "cannot chdir to " + romDir;
				inst->bootFailed = true;
				return inst;
			}

			const auto rom = xt::RomLoader::findROM();
			if(!rom.isValid())
			{
				inst->lastError = "no valid Xenia ROM found in " + romDir;
				inst->bootFailed = true;
				return inst;
			}

			inst->device = new xt::Xt(rom.getData(), rom.getFilename());
			if(!inst->device->isValid())
			{
				inst->lastError = "device failed to construct from ROM";
				inst->bootFailed = true;
				return inst;
			}

			// Boot synchronously. xenia_render measured ~1M blocks worst
			// case on real Push 3 hardware (~0.1-0.2s at the CPU speeds
			// measured there) -- fast enough to do inline at instance
			// creation without a timeout here; if this ever hangs, that is
			// itself the signal something is wrong, the same as it would be
			// in xenia_render.
			while(!inst->device->isBootCompleted())
				inst->device->process(kNativeBlock);

			inst->resampleFactor = static_cast<double>(g_hostSampleRate) / kNativeRate;
			inst->resampL.open(inst->resampleFactor);
			inst->resampR.open(inst->resampleFactor);

			for(const auto &p : kParams)
				inst->paramValues[p.key] = p.defaultV;

			// Hypothesis fix for "moving parameters does nothing except
			// program change": Global Parameter index 27 ("Parameter
			// receive" per the SysEx spec's GDATA table) gates whether the
			// device accepts incoming MIDI parameter changes at all. If it
			// defaults to off, every CC-based set_param below would be
			// silently ignored while Program Change (an ordinary channel-
			// voice message, unrelated to this gate) keeps working --
			// exactly the reported symptom. Force it on at boot. NOT yet
			// confirmed against real hardware; if params still don't
			// respond after this, this wasn't the cause and the CC chart
			// mapping itself needs re-checking instead. A few extra
			// process() calls give the device time to consume the queued
			// SysEx before returning (sendMidiEvent only queues it).
			sendGlobalParamChange(inst, 27, 1);
			for(int i = 0; i < 50; ++i)
				inst->device->process(kNativeBlock);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("create_instance exception: ") + e.what();
			inst->bootFailed = true;
		}

		return inst;
	}

	void xenia_destroy_instance(void *instance)
	{
		delete static_cast<XeniaInstance *>(instance);
	}

	void xenia_on_midi(void *instance, const uint8_t *msg, int len, int /*source*/)
	{
		auto *inst = static_cast<XeniaInstance *>(instance);
		if(inst->bootFailed || len < 1)
			return;

		const uint8_t status = msg[0];
		const uint8_t hi = status & 0xF0;
		const uint8_t data1 = len > 1 ? msg[1] : 0;
		const uint8_t data2 = len > 2 ? msg[2] : 0;

		try
		{
			// push-xenia (like push-braids before it) never forwards CC
			// here -- Push's encoders are consumed entirely inside the Go
			// host for its own on-screen param UI, which drives program/
			// cutoff/resonance through set_param below instead (see
			// params.go's applyEncoder -> bridge_plugin_set_param). A raw
			// CC could still arrive from some other future caller (e.g. an
			// external MIDI controller plugged into a different host), so
			// it's still forwarded like any other channel-voice message --
			// just no special-cased interception of specific CC numbers
			// here, since nothing currently sends them.
			uint8_t outStatus = status;
			if(hi >= 0x80 && hi <= 0xE0)
				outStatus = static_cast<uint8_t>(hi | kWorkingChannel0Indexed);

			if(hi == 0x90 && data2 > 0)
				trackNoteOn(inst, data1);
			else if(hi == 0x80 || (hi == 0x90 && data2 == 0))
				trackNoteOff(inst, data1);

			sendToDevice(inst, outStatus, data1, data2);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("on_midi exception: ") + e.what();
		}
	}

	void xenia_set_param(void *instance, const char *key, const char *val)
	{
		auto *inst = static_cast<XeniaInstance *>(instance);
		if(inst->bootFailed || !key || !val)
			return;

		// Reserved key (leading underscore, never a real param) -- the Go
		// host calls this once it knows the audio session's actual
		// negotiated sample rate, which is only ever known after Live
		// opens its side and isn't available yet at plugin-load time (see
		// main.go's pluginInitRate). Without this, resampleFactor stays
		// permanently wrong whenever the negotiated rate isn't exactly
		// pluginInitRate -- audio still plays, just pitched/timestretched
		// by the ratio between the two, and tickNoteWatchdog's elapsed-time
		// math (which also reads g_hostSampleRate) runs equally wrong,
		// delaying its stuck-note release far past kMaxNoteHoldSeconds.
		if(strcmp(key, "_host_sample_rate") == 0)
		{
			const int rate = atoi(val);
			if(rate > 0)
			{
				g_hostSampleRate = static_cast<uint32_t>(rate);
				inst->resampleFactor = static_cast<double>(rate) / kNativeRate;
				fprintf(stdout, "[xenia_plugin] _host_sample_rate=%d -> resampleFactor=%f (kNativeRate=%f)\n",
					rate, inst->resampleFactor, kNativeRate);
				inst->resampL.reopen(inst->resampleFactor);
				inst->resampR.reopen(inst->resampleFactor);
			}
			return;
		}

		const ParamDef *def = findParam(key);
		if(!def)
			return; // unknown key -- no-op, same as before

		const int raw = atoi(val);
		const uint8_t v = static_cast<uint8_t>(raw < def->minV ? def->minV : (raw > def->maxV ? def->maxV : raw));

		try
		{
			// No debug fprintf here on purpose: xenia_set_param runs on the
			// same goroutine/thread that renders audio (see midiHandler's
			// doc comment in main.go), and a blocking, unbuffered stdout
			// write on every single param change (every encoder detent)
			// was an audible micro-stutter on real hardware -- confirmed
			// by the same mechanism as xtPic.cpp's MCLOG flood (see the
			// mc68k::logToConsole override above), just triggered by our
			// own debug logging instead of gearmulator's.
			inst->paramValues[key] = v;
			if(const SysexParamDef *sp = findSysexParam(key))
			{
				sendSingleParamChange(inst, sp->sdataIndex, v);
			}
			else if(def->outCC == kProgramSentinel)
			{
				sendToDevice(inst, static_cast<uint8_t>(0xC0 | kWorkingChannel0Indexed), v, 0);
				// A Program Change silently replaces the device's entire
				// current patch -- request the edit buffer back so
				// paramValues (and thus this host's on-screen/web knob
				// positions, once params.go's syncFromPluginState re-reads
				// "state") reflect what the new patch actually set instead
				// of whatever the previous one left behind. See
				// drainDeviceMidiOut/parseSingleDump for the response side.
				sendEditBufferRequest(inst);
			}
			else if(def->outCC != 0)
			{
				sendToDevice(inst, static_cast<uint8_t>(0xB0 | kWorkingChannel0Indexed), def->outCC, v);
			}
			// outCC == 0: tracked on screen/web UI only -- see kParams'
			// header comment for why (no verified CC to send yet).
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("set_param exception: ") + e.what();
		}
	}

	int xenia_get_param(void *instance, const char *key, char *buf, int buf_len)
	{
		auto *inst = static_cast<XeniaInstance *>(instance);
		auto write = [&](const std::string &v) -> int
		{
			const int n = static_cast<int>(v.size());
			if(n >= buf_len) return -1;
			memcpy(buf, v.c_str(), static_cast<size_t>(n) + 1);
			return n;
		};
		if(strcmp(key, "engine_name") == 0) return write("Xenia (Waldorf Microwave XT)");
		if(strcmp(key, "engine") == 0) return write("0");
		if(strcmp(key, "chain_params") == 0)
		{
			// Host (push-hack-braids/src/params.go: paramMeta) requires a
			// JSON array of {key,name,type,min,max,options} objects, not
			// bare strings -- a bare-string array here was silently
			// crash-looping the plugin on every load (json.Unmarshal into
			// []paramMeta failing on every entry).
			std::string json = "[";
			for(size_t i = 0; i < kNumParams; ++i)
			{
				const auto &p = kParams[i];
				if(i) json += ",";
				json += "{\"key\":\"" + std::string(p.key) + "\",\"name\":\"" + p.name +
					"\",\"type\":\"int\",\"min\":" + std::to_string(p.minV) +
					",\"max\":" + std::to_string(p.maxV) + ",\"options\":[]}";
			}
			json += "]";
			return write(json);
		}
		if(strcmp(key, "lcd_patch_id") == 0)
			return write(lastToken(firstField(g_lcdLine1)));
		if(strcmp(key, "lcd_patch_name") == 0)
			return write(firstField(g_lcdLine2));
		if(strcmp(key, "state") == 0)
		{
			// Read by the host right after instance creation (and after any
			// preset load) to resync its on-screen values -- see
			// syncFromPluginState in params.go. A missing/failing "state"
			// isn't fatal there (just a log line), but answering it keeps
			// every param in sync if the host ever surfaces them.
			std::string json = "{";
			bool first = true;
			for(const auto &kv : inst->paramValues)
			{
				if(!first) json += ",";
				first = false;
				json += "\"" + kv.first + "\":" + std::to_string(kv.second);
			}
			json += "}";
			return write(json);
		}
		if(const ParamDef *def = findParam(key))
		{
			auto it = inst->paramValues.find(key);
			const uint8_t v = it != inst->paramValues.end() ? it->second : def->defaultV;
			return write(std::to_string(v));
		}
		return -1;
	}

	int xenia_get_error(void *instance, char *buf, int buf_len)
	{
		auto *inst = static_cast<XeniaInstance *>(instance);
		if(inst->lastError.empty()) return 0;
		const int n = static_cast<int>(inst->lastError.size());
		if(n >= buf_len) return -1;
		memcpy(buf, inst->lastError.c_str(), static_cast<size_t>(n) + 1);
		return n;
	}

	void xenia_render_block(void *instance, int16_t *out_interleaved_lr, int frames)
	{
		auto *inst = static_cast<XeniaInstance *>(instance);
		if(inst->bootFailed || frames <= 0)
		{
			if(frames > 0) memset(out_interleaved_lr, 0, static_cast<size_t>(frames) * 2 * sizeof(int16_t));
			return;
		}

		try
		{
			while(static_cast<int>(inst->resampL.pending.size()) < frames)
				fillMoreOutput(inst);

			for(int i = 0; i < frames; ++i)
			{
				auto clamp16 = [](float f) -> int16_t
				{
					const float scaled = f * kOutputHeadroom * 32767.0f;
					if(scaled > 32767.0f) return 32767;
					if(scaled < -32768.0f) return -32768;
					return static_cast<int16_t>(scaled);
				};
				out_interleaved_lr[i * 2]     = clamp16(inst->resampL.pending[static_cast<size_t>(i)]);
				out_interleaved_lr[i * 2 + 1] = clamp16(inst->resampR.pending[static_cast<size_t>(i)]);
			}

			inst->resampL.pending.erase(inst->resampL.pending.begin(), inst->resampL.pending.begin() + frames);
			inst->resampR.pending.erase(inst->resampR.pending.begin(), inst->resampR.pending.begin() + frames);

			// Proactive lookahead top-up -- spreads the "just-in-time"
			// render burst that caused the 22 slow-block warnings in the
			// first hardware run across every call instead of only the
			// one call unlucky enough to land right when a note event
			// needs a fresh native block.
			while(static_cast<int>(inst->resampL.pending.size()) < kLookaheadFrames)
				fillMoreOutput(inst);

			tickNoteWatchdog(inst, frames);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("render_block exception: ") + e.what();
			inst->bootFailed = true;
			memset(out_interleaved_lr, 0, static_cast<size_t>(frames) * 2 * sizeof(int16_t));
		}
	}

	plugin_api_v2_t g_api = {
		2,
		xenia_create_instance,
		xenia_destroy_instance,
		xenia_on_midi,
		xenia_set_param,
		xenia_get_param,
		xenia_get_error,
		xenia_render_block,
	};
}

extern "C" __attribute__((visibility("default")))
plugin_api_v2_t *move_plugin_init_v2(const host_api_v1_t *host)
{
	g_hostSampleRate = host && host->sample_rate > 0
		? static_cast<uint32_t>(host->sample_rate)
		: 44100;

	return &g_api;
}
