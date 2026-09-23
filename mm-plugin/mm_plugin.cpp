/*
 * mm_plugin.cpp — Elektron Monomachine emulation (gearmulator-md-mm's
 * mdLib, MachineModel::Monomachine) as a Move Anything plugin_api_v2
 * module, for push-hack-mm's generic host (src/bridge.c dlopen's any .so
 * exporting move_plugin_init_v2 — nothing about that host is Xenia/MM-
 * specific; see push-hack-xenia/src/bridge.c's own header comment, which
 * this bridge reuses unmodified).
 *
 * Unlike xenia_plugin.cpp (which wraps xtLib's xt::Xt, a synth-specific
 * convenience class with its own process()/sendMidiEvent()/
 * getAudioOutputs() helpers), mdLib exposes md::Device, a subclass of the
 * generic synthLib::Device base — process() takes raw
 * TAudioInputs/TAudioOutputs float pointers plus a MIDI in/out event
 * vector directly, and there is no per-synth resampling to do: both
 * Machinedrum and Monomachine run their codec path at a fixed 44100Hz
 * (mdLib/mdtypes.h's g_samplerate), which matches this host's own PCM
 * rate, so (unlike Xenia's 40kHz-native + libresample dance) this bridge
 * never resamples.
 *
 * Two independent control surfaces reach the emulated hardware here,
 * matching how the real machine actually offers external control:
 *
 *   1. MIDI CC "parameter automation" — mdLib/mdautomation.h's
 *      encodeParameterChange, transcribed directly from Elektron's own
 *      published MIDI implementation (one MIDI channel per track,
 *      channel = baseChannel + track; a page/index pair maps to a fixed
 *      CC number per mmController() in mdautomation.cpp). This is what
 *      set_param uses for the 58 per-track synth params (Synthesis A-H,
 *      Amp/Filter/Effects/LFO1-3 pages, Level, Mute) — the real
 *      hardware's documented "control every param over MIDI" feature,
 *      same category as Xenia's transcribed CC chart.
 *   2. Front-panel simulation — mdLib/mdpanel.h's PanelControl/
 *      panelPacket/PanelRowState, driving Device::sendPanelEvent exactly
 *      as if a human pressed a physical button. This is the ONLY way to
 *      reach the 16 trig keys, the 6 track-select buttons, transport
 *      (Play/Stop/Record), and page/bank navigation — none of those have
 *      a MIDI-CC equivalent on real hardware, so there is nothing to
 *      transcribe; this bridge drives the emulated panel UART the same
 *      way a finger would. panel_button/panel_track/toggle_step below are
 *      the three synthetic set_param keys that reach it.
 *
 * ASSUMPTION FLAGGED FOR HARDWARE VERIFICATION (see docs/monomachine-
 * port-notes.md's "Known unverified" section): toggle_step below presses
 * and releases a Trigger key as one quick tap to flip that step's on/off
 * state, on the assumption that basic grid step-programming needs no
 * separate "grid record" mode on real Monomachine hardware (unlike RECORD,
 * which is for live-recording played notes/knob moves). Not yet confirmed
 * against real hardware or the printed manual — the fallback if this
 * assumption is wrong is to also simulate holding PanelControl::Record
 * while tapping the trig key, a one-line change in toggleStep() below.
 */

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <exception>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "mdLib/mddevice.h"
#include "mdLib/mdpanel.h"
#include "mdLib/mdautomation.h"
#include "mdLib/mdromloader.h"
#include "mdLib/mdfrontpanel.h"
#include "synthLib/midiTypes.h"

#include <unistd.h>

namespace
{
	constexpr int kNumTracks = md::automation::monomachine::TrackCount; // 6
	constexpr int kNumSteps = 16;
	constexpr double kMaxNoteHoldSeconds = 60.0;

	// Safety ceiling, same reasoning as Xenia's kOutputHeadroom: this is a
	// hardware bring-up bridge, not a mastered signal chain.
	constexpr float kOutputHeadroom = 0.5f;

	uint32_t g_hostSampleRate = 44100;

	// -- per-track parameter table -------------------------------------
	// Transcribed directly from gearmulator-md-mm's own
	// source/elektron/md/mdJucePlugin/parameterDescriptions_mm.json (the
	// real JUCE plugin's own Monomachine parameter list) — page/index
	// match that file's "page"/"index" fields exactly, which is also
	// exactly what mdLib/mdautomation.cpp's mmController() expects to
	// encode as a CC. Not guessed or renamed.
	struct ParamDef
	{
		const char *key;
		const char *name;
		uint8_t page;
		uint8_t index;
		int minV;
		int maxV;
		uint8_t defaultV;
	};
	using md::automation::monomachine::Synthesis;
	using md::automation::monomachine::Amplification;
	using md::automation::monomachine::Filter;
	using md::automation::monomachine::Effects;
	using md::automation::monomachine::Lfo1;
	using md::automation::monomachine::Lfo2;
	using md::automation::monomachine::Lfo3;
	using md::automation::monomachine::Level;
	using md::automation::monomachine::Mute;

	constexpr ParamDef kParams[] = {
		// -- SYNTHESIS page (the 8 machine-dependent synthesis params A-H) --
		{"synth_a", "Synthesis A", Synthesis, 0, 0, 127, 64},
		{"synth_b", "Synthesis B", Synthesis, 1, 0, 127, 64},
		{"synth_c", "Synthesis C", Synthesis, 2, 0, 127, 64},
		{"synth_d", "Synthesis D", Synthesis, 3, 0, 127, 64},
		{"synth_e", "Synthesis E", Synthesis, 4, 0, 127, 64},
		{"synth_f", "Synthesis F", Synthesis, 5, 0, 127, 64},
		{"synth_g", "Synthesis G", Synthesis, 6, 0, 127, 64},
		{"synth_h", "Synthesis H", Synthesis, 7, 0, 127, 64},
		// -- AMPLIFICATION page --
		{"amp_attack",      "Amp Attack",      Amplification, 0, 0, 127, 0},
		{"amp_hold",        "Amp Hold",        Amplification, 1, 0, 127, 0},
		{"amp_decay",       "Amp Decay",       Amplification, 2, 0, 127, 64},
		{"amp_release",     "Amp Release",     Amplification, 3, 0, 127, 32},
		{"amp_distortion",  "Amp Distortion",  Amplification, 4, 0, 127, 0},
		{"amp_volume",      "Amp Volume",      Amplification, 5, 0, 127, 100},
		{"amp_pan",         "Amp Pan",         Amplification, 6, 0, 127, 64},
		{"amp_portamento",  "Amp Portamento",  Amplification, 7, 0, 127, 0},
		// -- FILTER page --
		{"filter_base",         "Filter Base",         Filter, 0, 0, 127, 100},
		{"filter_width",        "Filter Width",        Filter, 1, 0, 127, 64},
		{"filter_hp_q",         "Filter Highpass Q",   Filter, 2, 0, 127, 0},
		{"filter_lp_q",         "Filter Lowpass Q",    Filter, 3, 0, 127, 0},
		{"filter_attack",       "Filter Attack",       Filter, 4, 0, 127, 0},
		{"filter_decay",        "Filter Decay",        Filter, 5, 0, 127, 64},
		{"filter_base_offset",  "Filter Base Offset",  Filter, 6, 0, 127, 64},
		{"filter_width_offset", "Filter Width Offset", Filter, 7, 0, 127, 64},
		// -- EFFECTS page --
		{"fx_eq_freq",       "Effects EQ Freq",       Effects, 0, 0, 127, 64},
		{"fx_eq_gain",       "Effects EQ Gain",       Effects, 1, 0, 127, 64},
		{"fx_srr",           "Effects Sample Rate Reduction", Effects, 2, 0, 127, 0},
		{"fx_delay_time",    "Effects Delay Time",    Effects, 3, 0, 127, 0},
		{"fx_delay_send",    "Effects Delay Send",    Effects, 4, 0, 127, 0},
		{"fx_delay_feedback","Effects Delay Feedback",Effects, 5, 0, 127, 0},
		{"fx_delay_base",    "Effects Delay Base",    Effects, 6, 0, 127, 64},
		{"fx_delay_width",   "Effects Delay Width",   Effects, 7, 0, 127, 64},
		// -- LFO 1/2/3 pages (identical shape, 3 independent LFOs) --
		{"lfo1_page",      "LFO 1 Page",      Lfo1, 0, 0, 127, 0},
		{"lfo1_dest",      "LFO 1 Destination", Lfo1, 1, 0, 127, 0},
		{"lfo1_trig",      "LFO 1 Trigger",   Lfo1, 2, 0, 127, 0},
		{"lfo1_wave",      "LFO 1 Waveform",  Lfo1, 3, 0, 127, 0},
		{"lfo1_mult",      "LFO 1 Multiplier",Lfo1, 4, 0, 127, 0},
		{"lfo1_speed",     "LFO 1 Speed",     Lfo1, 5, 0, 127, 64},
		{"lfo1_interlace", "LFO 1 Interlace", Lfo1, 6, 0, 127, 0},
		{"lfo1_depth",     "LFO 1 Depth",     Lfo1, 7, 0, 127, 0},
		{"lfo2_page",      "LFO 2 Page",      Lfo2, 0, 0, 127, 0},
		{"lfo2_dest",      "LFO 2 Destination", Lfo2, 1, 0, 127, 0},
		{"lfo2_trig",      "LFO 2 Trigger",   Lfo2, 2, 0, 127, 0},
		{"lfo2_wave",      "LFO 2 Waveform",  Lfo2, 3, 0, 127, 0},
		{"lfo2_mult",      "LFO 2 Multiplier",Lfo2, 4, 0, 127, 0},
		{"lfo2_speed",     "LFO 2 Speed",     Lfo2, 5, 0, 127, 64},
		{"lfo2_interlace", "LFO 2 Interlace", Lfo2, 6, 0, 127, 0},
		{"lfo2_depth",     "LFO 2 Depth",     Lfo2, 7, 0, 127, 0},
		{"lfo3_page",      "LFO 3 Page",      Lfo3, 0, 0, 127, 0},
		{"lfo3_dest",      "LFO 3 Destination", Lfo3, 1, 0, 127, 0},
		{"lfo3_trig",      "LFO 3 Trigger",   Lfo3, 2, 0, 127, 0},
		{"lfo3_wave",      "LFO 3 Waveform",  Lfo3, 3, 0, 127, 0},
		{"lfo3_mult",      "LFO 3 Multiplier",Lfo3, 4, 0, 127, 0},
		{"lfo3_speed",     "LFO 3 Speed",     Lfo3, 5, 0, 127, 64},
		{"lfo3_interlace", "LFO 3 Interlace", Lfo3, 6, 0, 127, 0},
		{"lfo3_depth",     "LFO 3 Depth",     Lfo3, 7, 0, 127, 0},
		// -- LEVEL / MUTE (single-value "pages", per real hardware) --
		{"level", "Level", Level, 0, 0, 127, 100},
		{"mute",  "Mute",  Mute,  0, 0, 1,   0},
	};
	constexpr size_t kNumParams = sizeof(kParams) / sizeof(kParams[0]);

	const ParamDef *findParam(const char *key)
	{
		for(const auto &p : kParams)
			if(strcmp(p.key, key) == 0)
				return &p;
		return nullptr;
	}

	// panelControlByName resolves the button names used by set_param's
	// "panel_button" key. Only the subset a Push-grid host plausibly needs
	// is listed here — every name is one of PanelControl's own enumerators
	// (mdLib/mdpanel.h), spelled identically, so panelControlName()'s
	// output (useful for logging) round-trips through this table.
	const std::map<std::string, md::PanelControl> &panelControlNames()
	{
		static const std::map<std::string, md::PanelControl> table = {
			{"Play", md::PanelControl::Play},
			{"Stop", md::PanelControl::Stop},
			{"Record", md::PanelControl::Record},
			{"Function", md::PanelControl::Function},
			{"Kit", md::PanelControl::Kit},
			{"Enter", md::PanelControl::Enter},
			{"Exit", md::PanelControl::Exit},
			{"Up", md::PanelControl::Up},
			{"Down", md::PanelControl::Down},
			{"Left", md::PanelControl::Left},
			{"Right", md::PanelControl::Right},
			{"BankGroup", md::PanelControl::BankGroup},
			{"BankA", md::PanelControl::BankA},
			{"BankB", md::PanelControl::BankB},
			{"BankC", md::PanelControl::BankC},
			{"BankD", md::PanelControl::BankD},
			{"Scale", md::PanelControl::Scale},
			{"TrigSelect", md::PanelControl::TrigSelect},
			{"SongEnable", md::PanelControl::SongEnable},
			{"DataPageForward", md::PanelControl::DataPageForward},
			{"DataPageBackward", md::PanelControl::DataPageBackward},
			{"Tempo", md::PanelControl::Tempo},
		};
		return table;
	}

	md::PanelControl trackControl(int track)
	{
		return static_cast<md::PanelControl>(
			static_cast<uint8_t>(md::PanelControl::Track1) + track);
	}

	md::PanelControl triggerControl(int step)
	{
		return static_cast<md::PanelControl>(
			static_cast<uint8_t>(md::PanelControl::Trigger1) + step);
	}

	// -- base64, self-contained (no third-party dep pulled in just for
	// this) -- used only for panel_state's LCD framebuffer field.
	std::string base64Encode(const uint8_t *data, size_t len)
	{
		static const char table[] =
			"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
		std::string out;
		out.reserve(((len + 2) / 3) * 4);
		size_t i = 0;
		for(; i + 3 <= len; i += 3)
		{
			uint32_t n = (uint32_t(data[i]) << 16) | (uint32_t(data[i + 1]) << 8) | data[i + 2];
			out += table[(n >> 18) & 0x3F];
			out += table[(n >> 12) & 0x3F];
			out += table[(n >> 6) & 0x3F];
			out += table[n & 0x3F];
		}
		if(len - i == 1)
		{
			uint32_t n = uint32_t(data[i]) << 16;
			out += table[(n >> 18) & 0x3F];
			out += table[(n >> 12) & 0x3F];
			out += "==";
		}
		else if(len - i == 2)
		{
			uint32_t n = (uint32_t(data[i]) << 16) | (uint32_t(data[i + 1]) << 8);
			out += table[(n >> 18) & 0x3F];
			out += table[(n >> 12) & 0x3F];
			out += table[(n >> 6) & 0x3F];
			out += '=';
		}
		return out;
	}

	struct ActiveNote
	{
		uint8_t channel = 0;
		uint8_t note = 0;
		double heldSeconds = 0.0;
	};

	// PresetJob: one save-or-load in flight at a time. The heavy parts
	// (file I/O, building a whole replacement md::Hardware in
	// StateTransaction::prepare(), destroying the retired one) run on
	// `worker`; only the O(1) device touches -- beginStateTransaction and
	// finishStateTransaction's hardware swap -- happen on the render
	// thread, same split mdJucePlugin uses under its device lock. `stage`
	// is the only handoff between the two threads.
	struct PresetJob
	{
		enum Stage : int
		{
			Idle,
			Reading,   // worker: reading + validating the file
			ReadDone,  // render: beginStateTransaction
			Begun,     // worker: prepare()
			Prepared,  // render: fade out one block, then commit
			Finished,  // worker: destroy retired hardware, report status
			Aborted,   // worker: report status
			Saving,    // worker: writing the file
		};
		std::atomic<int> stage{Idle};
		std::atomic<bool> quit{false};
		std::thread worker;
		std::shared_ptr<const std::vector<uint8_t>> blob;
		std::vector<uint8_t> shadow;
		std::string extra;
		std::unique_ptr<synthLib::Device::StateTransaction> tx;
		bool fading = false;
		bool committed = false;
		std::mutex statusMu;
		std::string status = "idle";

		void setStatus(std::string s)
		{
			std::lock_guard<std::mutex> lock(statusMu);
			status = std::move(s);
		}
		std::string getStatus()
		{
			std::lock_guard<std::mutex> lock(statusMu);
			return status;
		}
		bool loadInFlight() const
		{
			const int s = stage.load(std::memory_order_acquire);
			return s >= Reading && s <= Finished;
		}
		// Worker-side wait for the render thread to move `stage` off `s`.
		// Returns false on shutdown (destroy_instance) so the worker never
		// blocks forever on a render thread that has stopped calling in.
		bool waitWhile(int s)
		{
			while(stage.load(std::memory_order_acquire) == s)
			{
				if(quit.load(std::memory_order_acquire))
					return false;
				std::this_thread::sleep_for(std::chrono::milliseconds(2));
			}
			return true;
		}
	};

	struct MmInstance
	{
		md::Device *device = nullptr;
		bool bootFailed = false;
		std::string lastError;

		int currentTrack = 0;
		int baseChannel = 0; // 0-indexed: track N's notes/CCs use channel baseChannel+N

		// Real (and this JIT-emulated) Monomachine firmware ignores MIDI/panel
		// input for several seconds after power-on while its own boot sequence
		// runs -- confirmed via gearmulator-md-mm's own
		// mdLibTest/mmBootFirmwareTest.cpp and mmAudioFirmwareTest.cpp, which
		// both require advance(hardware, g_samplerate * 20) before treating the
		// device as ready for MIDI. A note/CC/panel event sent before that
		// point is silently dropped by the still-booting emulated UART, not
		// queued -- reproduced directly against the real ROM (dlopen smoke
		// test: identical note+CC sent at t=0 produced total silence across
		// 28s of rendered audio; the same call sent after a 22s warm-up
		// produced normal output). See kBootFrames/isBooting below.
		uint64_t framesSinceCreate = 0;

		md::PanelRowState panelRows;

		// One value per kParams entry, per track — this bridge's own
		// shadow of what it has sent, same limitation as xenia_plugin.cpp's
		// paramValues: mdLib has no "read back a single live synth param"
		// call, only a full binary patch-RAM state dump, so this map (not
		// the device) is the source of truth the host's on-screen/web UI
		// reads from. Reset to kParams' defaultV per track at boot.
		std::array<std::map<std::string, uint8_t>, kNumTracks> paramValues;

		// pendingMidiIn is drained into device->process() on the next
		// render_block call — md::Device (unlike xtLib's xt::Xt) has no
		// standalone sendMidiEvent(); MIDI only reaches it via process()'s
		// _midiIn vector.
		std::vector<synthLib::SMidiEvent> pendingMidiIn;

		std::vector<ActiveNote> activeNotes;

		// pendingPanelReleases: see tapControl's doc comment -- a panel
		// button's release is deferred to a later render_block call
		// instead of happening synchronously right after the press, so
		// the emulated firmware's key-scan gets genuine elapsed
		// device-time to observe the press before it's released.
		struct PendingRelease { md::PanelControl control; uint64_t releaseAtFrame; };
		std::vector<PendingRelease> pendingPanelReleases;

		// render_block's float scratch, sized once in create_instance so
		// the render thread never allocates.
		std::vector<float> left, right;
		std::vector<synthLib::SMidiEvent> midiOut;

		PresetJob preset;
		// Opaque host payload stored alongside the last loaded/saved
		// preset (push-hack-mm keeps its SEQ step shadow here).
		std::string presetExtra;
		uint32_t presetLoads = 0; // bumped on every committed load

		~MmInstance()
		{
			// Join before deleting device: the worker may still hold a
			// transaction referencing the device's preparation context.
			preset.quit.store(true, std::memory_order_release);
			if(preset.worker.joinable())
				preset.worker.join();
			delete device;
		}
	};

	// kBootSeconds: 2s margin over the 20s gearmulator-md-mm's own firmware
	// tests require before treating a fresh device as ready for MIDI/panel
	// input (mmBootFirmwareTest.cpp/mmAudioFirmwareTest.cpp's own
	// advance(hardware, g_samplerate * 20)). g_hostSampleRate is fixed by the
	// time any instance exists (set once in move_plugin_init_v2).
	constexpr uint32_t kBootSeconds = 22;

	bool isBooting(const MmInstance *inst)
	{
		return inst->framesSinceCreate < static_cast<uint64_t>(kBootSeconds) * g_hostSampleRate;
	}

	// kPanelHoldFrames: matches gearmulator-md-mm's own mdLibTest tap()
	// helper, which always advances 2048 samples of real device-time
	// between a panel press and its release -- see tapControl's doc
	// comment for why this bridge needs the same gap, deferred across
	// render_block calls rather than done synchronously.
	constexpr uint32_t kPanelHoldFrames = 2048;

	void queueMidi(MmInstance *inst, uint8_t status, uint8_t d1, uint8_t d2)
	{
		inst->pendingMidiIn.emplace_back(synthLib::MidiEventSource::Host, status, d1, d2);
	}

	void queueMidi3(MmInstance *inst, const md::automation::ControlChange &cc)
	{
		queueMidi(inst, cc[0], cc[1], cc[2]);
	}

	void trackNoteOn(MmInstance *inst, uint8_t channel, uint8_t note)
	{
		for(auto &n : inst->activeNotes)
			if(n.channel == channel && n.note == note) { n.heldSeconds = 0.0; return; }
		inst->activeNotes.push_back({channel, note, 0.0});
	}

	void trackNoteOff(MmInstance *inst, uint8_t channel, uint8_t note)
	{
		auto &v = inst->activeNotes;
		v.erase(std::remove_if(v.begin(), v.end(), [&](const ActiveNote &n) {
			return n.channel == channel && n.note == note;
		}), v.end());
	}

	void tickNoteWatchdog(MmInstance *inst, size_t frames)
	{
		if(inst->activeNotes.empty() || g_hostSampleRate == 0)
			return;
		const double elapsed = static_cast<double>(frames) / static_cast<double>(g_hostSampleRate);
		for(auto it = inst->activeNotes.begin(); it != inst->activeNotes.end(); )
		{
			it->heldSeconds += elapsed;
			if(it->heldSeconds >= kMaxNoteHoldSeconds)
			{
				queueMidi(inst, static_cast<uint8_t>(0x80 | it->channel), it->note, 0);
				it = inst->activeNotes.erase(it);
			}
			else
			{
				++it;
			}
		}
	}

	// pressControl/releaseControl merge _control's packet through inst's
	// PanelRowState (so a control sharing a scan row with something else
	// already held doesn't clobber it — see mdpanel.h's PanelRowState doc)
	// and forward the merged row/mask to the emulated panel UART.
	bool pressControl(MmInstance *inst, md::PanelControl control)
	{
		auto packet = md::panelPacket(md::MachineModel::Monomachine, control);
		if(!packet)
			return false;
		auto merged = inst->panelRows.press(*packet);
		return inst->device->sendPanelEvent(merged.row, merged.mask);
	}

	bool releaseControl(MmInstance *inst, md::PanelControl control)
	{
		auto packet = md::panelPacket(md::MachineModel::Monomachine, control);
		if(!packet)
			return false;
		auto merged = inst->panelRows.release(*packet);
		return inst->device->sendPanelEvent(merged.row, merged.mask);
	}

	// tapControl presses now, releases kPanelHoldFrames of real
	// device-time later (drained by drainPendingPanelReleases, called at
	// the top of every mm_render_block) — the panel-simulation
	// equivalent of a quick physical button tap (Play/Stop/track-select/
	// trig-key toggle). Buttons meant to be *held* (e.g. Record while
	// turning a knob for a parameter lock) instead need separate
	// panel_button "...,down"/"...,up" calls from the Go host — see
	// set_param's "panel_button" handling below.
	//
	// Releasing synchronously (press+release with zero elapsed
	// device-time, in the same call) is what this file used to do, and
	// it does NOT reliably work: a real-ROM dlopen test showed a
	// synchronous toggleStep's step LED never actually flip, even though
	// the LCD reacted (so the panel UART received something, just not a
	// registered keypress) -- see git history / docs/
	// monomachine-hardware-parity-todo.md. gearmulator-md-mm's own
	// mdLibTest tap() helper always advances real device time (2048
	// samples) between a press and its release; this defers the release
	// the same way, just spread across render_block calls instead of a
	// blocking sleep, since mm_render_block is the only place this
	// bridge may legitimately advance device time (see
	// MmInstance::pendingPanelReleases' doc comment).
	void tapControl(MmInstance *inst, md::PanelControl control)
	{
		pressControl(inst, control);
		inst->pendingPanelReleases.push_back({control, inst->framesSinceCreate + kPanelHoldFrames});
	}

	// drainPendingPanelReleases releases any panel control whose held
	// time has elapsed — called at the top of every mm_render_block,
	// before that block's own framesSinceCreate advance, so a control
	// tapped this same call doesn't get released prematurely.
	void drainPendingPanelReleases(MmInstance *inst)
	{
		auto &pending = inst->pendingPanelReleases;
		for(auto it = pending.begin(); it != pending.end(); )
		{
			if(it->releaseAtFrame <= inst->framesSinceCreate)
			{
				releaseControl(inst, it->control);
				it = pending.erase(it);
			}
			else
			{
				++it;
			}
		}
	}

	// selectTrack switches inst->currentTrack and, unless already current,
	// taps that track's front-panel Track button so the emulated
	// firmware's own notion of "selected track" (which drives its LCD,
	// its Data Entry A-H targets, and getMonomachineStepLedColor's
	// readback) stays in lockstep with this bridge's idea of it. Every
	// code path that changes currentTrack goes through this, so the two
	// can never drift.
	void selectTrack(MmInstance *inst, int track)
	{
		if(track < 0 || track >= kNumTracks)
			return;
		if(track != inst->currentTrack)
			tapControl(inst, trackControl(track));
		inst->currentTrack = track;
	}

	// toggleStep is the pad-grid grid-editing primitive: switches to
	// _track (if not already selected — see selectTrack) then taps that
	// step's trig key while Grid Record is held. Real Monomachine gates
	// trig-key step-editing behind Grid Record mode (Record's own red
	// LED) -- confirmed via docs/monomachine-hardware-parity-todo.md's
	// research citing Elektron's own quick-start guide ("You enter Grid
	// mode by hitting the Record button, whereupon its red LED will
	// light"). Record is pressed immediately and released a little later
	// than the trigger key's own deferred release (see tapControl's doc
	// comment) so it stays held across the trigger's entire press+release
	// window, matching how a human would actually perform this gesture.
	void toggleStep(MmInstance *inst, int track, int step)
	{
		if(step < 0 || step >= kNumSteps)
			return;
		selectTrack(inst, track);
		pressControl(inst, md::PanelControl::Record);
		tapControl(inst, triggerControl(step));
		inst->pendingPanelReleases.push_back(
			{md::PanelControl::Record, inst->framesSinceCreate + kPanelHoldFrames + 256});
	}

	// applyTrackParam sets one of kParams for an explicit track (not
	// necessarily inst->currentTrack) via CC automation — unlike a panel
	// button, a CC carries its own destination channel (baseChannel+track)
	// in the message itself, so this does NOT need to select the track
	// first the way toggleStep must for the panel-simulated trig key.
	// Shared by mm_set_param's normal per-key dispatch (track =
	// currentTrack) and toggle_mute (track = whatever the SEQ page's
	// mute-row pad named, independent of which track is currently
	// selected for the A-H encoders).
	void applyTrackParam(MmInstance *inst, int track, const ParamDef *def, uint8_t v)
	{
		inst->paramValues[track][def->key] = v;
		md::automation::ParameterChange change{def->page, static_cast<uint8_t>(track), def->index, v};
		auto cc = md::automation::encodeParameterChange(
			md::MachineModel::Monomachine, change, static_cast<uint8_t>(inst->baseChannel));
		if(cc)
			queueMidi3(inst, *cc);
	}

	// toggleMute flips track's own shadowed "mute" value and sends it —
	// the SEQ page's mute-row primitive, mirroring toggleStep's shape but
	// for a plain CC param instead of a panel button.
	void toggleMute(MmInstance *inst, int track)
	{
		if(track < 0 || track >= kNumTracks)
			return;
		const ParamDef *def = findParam("mute");
		uint8_t cur = inst->paramValues[track]["mute"];
		applyTrackParam(inst, track, def, cur ? 0 : 1);
	}

	// -- presets ---------------------------------------------------------
	// File layout (little-endian u32 lengths):
	//   "PUMMA01\0" | u32 n | device state (md::Device::getState, Global:
	//   patch RAM + DigiPRO user flash) | u32 n | this bridge's per-track
	//   param shadow (kNumTracks x kNumParams bytes, kParams order) |
	//   u32 n | host extra (opaque)
	// The shadow rides along because mdLib can't read single params back:
	// without it every slider would show stale numbers after a recall.
	constexpr char kPresetMagic[8] = {'P', 'U', 'M', 'M', 'A', '0', '1', '\0'};

	bool writeChunk(FILE *f, const void *data, size_t n)
	{
		const uint8_t len[4] = {uint8_t(n), uint8_t(n >> 8), uint8_t(n >> 16), uint8_t(n >> 24)};
		return fwrite(len, 1, 4, f) == 4 && (n == 0 || fwrite(data, 1, n, f) == n);
	}

	bool readChunk(const std::vector<uint8_t> &file, size_t &pos, std::vector<uint8_t> &out)
	{
		if(file.size() - pos < 4)
			return false;
		const size_t n = size_t(file[pos]) | size_t(file[pos + 1]) << 8 |
			size_t(file[pos + 2]) << 16 | size_t(file[pos + 3]) << 24;
		pos += 4;
		if(file.size() - pos < n)
			return false;
		out.assign(file.begin() + long(pos), file.begin() + long(pos + n));
		pos += n;
		return true;
	}

	// Temp file + fsync + rename: a crash or power loss mid-save leaves the
	// previous preset intact, never a torn one.
	bool writePresetFile(const std::string &path, const std::vector<uint8_t> &blob,
		const std::vector<uint8_t> &shadow, const std::string &extra, std::string &err)
	{
		const std::string tmp = path + ".tmp";
		FILE *f = fopen(tmp.c_str(), "wb");
		if(!f)
		{
			err = "cannot open " + tmp;
			return false;
		}
		bool ok = fwrite(kPresetMagic, 1, sizeof(kPresetMagic), f) == sizeof(kPresetMagic) &&
			writeChunk(f, blob.data(), blob.size()) &&
			writeChunk(f, shadow.data(), shadow.size()) &&
			writeChunk(f, extra.data(), extra.size()) &&
			fflush(f) == 0 && fsync(fileno(f)) == 0;
		ok = fclose(f) == 0 && ok;
		if(!ok || rename(tmp.c_str(), path.c_str()) != 0)
		{
			remove(tmp.c_str());
			err = "write failed: " + path;
			return false;
		}
		return true;
	}

	bool readPresetFile(const std::string &path, std::vector<uint8_t> &blob,
		std::vector<uint8_t> &shadow, std::string &extra, std::string &err)
	{
		FILE *f = fopen(path.c_str(), "rb");
		if(!f)
		{
			err = "cannot open " + path;
			return false;
		}
		std::vector<uint8_t> file;
		uint8_t chunk[65536];
		size_t n;
		while((n = fread(chunk, 1, sizeof(chunk), f)) > 0)
			file.insert(file.end(), chunk, chunk + n);
		fclose(f);

		std::vector<uint8_t> extraBytes;
		size_t pos = sizeof(kPresetMagic);
		if(file.size() < pos || memcmp(file.data(), kPresetMagic, pos) != 0 ||
			!readChunk(file, pos, blob) || !readChunk(file, pos, shadow) ||
			!readChunk(file, pos, extraBytes) || blob.empty())
		{
			err = "not a puMMa preset (bad header or truncated): " + path;
			return false;
		}
		extra.assign(extraBytes.begin(), extraBytes.end());
		return true;
	}

	void loadWorker(MmInstance *inst, std::string path)
	{
		auto &job = inst->preset;
		std::vector<uint8_t> blob;
		std::string err;
		if(!readPresetFile(path, blob, job.shadow, job.extra, err))
		{
			job.setStatus("error: " + err);
			job.stage.store(PresetJob::Idle, std::memory_order_release);
			return;
		}
		job.blob = std::make_shared<const std::vector<uint8_t>>(std::move(blob));
		job.stage.store(PresetJob::ReadDone, std::memory_order_release);
		if(!job.waitWhile(PresetJob::ReadDone))
			return;
		if(job.stage.load(std::memory_order_acquire) == PresetJob::Begun)
		{
			// Builds a complete replacement md::Hardware from the state --
			// seconds of work, never on the render thread. Must not touch
			// the live device (synthLib::Device::StateTransaction contract).
			const bool prepared = job.tx->prepare();
			job.stage.store(PresetJob::Prepared, std::memory_order_release);
			if(!job.waitWhile(PresetJob::Prepared))
				return;
			if(!prepared)
				err = "device rejected preset state";
		}
		else
		{
			err = "device refused to start the restore";
		}
		// After a commit the transaction owns the retired hardware; its
		// destruction is heavy, so it happens here, not on the render thread.
		job.tx.reset();
		job.blob.reset();
		job.setStatus(job.committed ? "loaded " + path : "error: " + err);
		job.stage.store(PresetJob::Idle, std::memory_order_release);
	}

	void startLoad(MmInstance *inst, const std::string &path)
	{
		auto &job = inst->preset;
		if(job.stage.load(std::memory_order_acquire) != PresetJob::Idle)
		{
			job.setStatus("error: busy");
			return;
		}
		if(job.worker.joinable())
			job.worker.join(); // previous job already reached Idle: returns at once
		job.committed = false;
		job.fading = false;
		job.setStatus("loading " + path);
		job.stage.store(PresetJob::Reading, std::memory_order_release);
		job.worker = std::thread(loadWorker, inst, path);
	}

	// startSave snapshots device state on the calling (render) thread --
	// the device is not thread-safe, so this copy (~3 MiB: patch RAM +
	// DigiPRO flash) can't move off it -- and hands the file write to the
	// worker.
	void startSave(MmInstance *inst, const std::string &path, std::string extra)
	{
		auto &job = inst->preset;
		if(job.stage.load(std::memory_order_acquire) != PresetJob::Idle)
		{
			job.setStatus("error: busy");
			return;
		}
		auto blob = std::make_shared<std::vector<uint8_t>>();
		if(!inst->device->getState(*blob, synthLib::StateTypeGlobal) || blob->empty())
		{
			job.setStatus("error: device getState failed");
			return;
		}
		std::vector<uint8_t> shadow;
		shadow.reserve(kNumTracks * kNumParams);
		for(int t = 0; t < kNumTracks; ++t)
			for(const auto &p : kParams)
				shadow.push_back(inst->paramValues[t][p.key]);

		if(job.worker.joinable())
			job.worker.join();
		job.setStatus("saving " + path);
		job.stage.store(PresetJob::Saving, std::memory_order_release);
		inst->presetExtra = extra;
		job.worker = std::thread([inst, path, blob, shadow = std::move(shadow), extra = std::move(extra)]
		{
			std::string err;
			const bool ok = writePresetFile(path, *blob, shadow, extra, err);
			inst->preset.setStatus(ok ? "saved " + path : "error: " + err);
			inst->preset.stage.store(PresetJob::Idle, std::memory_order_release);
		});
	}

	// servicePresetLoad advances a load on the render thread; called at the
	// top of every render_block. Returns true when this block must fade
	// out (the next block commits, so the swap never cuts mid-waveform).
	bool servicePresetLoad(MmInstance *inst)
	{
		auto &job = inst->preset;
		const int stage = job.stage.load(std::memory_order_acquire);
		if(stage == PresetJob::ReadDone)
		{
			job.tx = inst->device->beginStateTransaction(job.blob, synthLib::StateTypeGlobal);
			job.stage.store(job.tx ? PresetJob::Begun : PresetJob::Aborted, std::memory_order_release);
			return false;
		}
		if(stage != PresetJob::Prepared)
			return false;
		if(!job.fading)
		{
			job.fading = true;
			return true;
		}
		// O(1) hardware swap; the retired machine is now owned by job.tx.
		job.committed = inst->device->finishStateTransaction(*job.tx);
		if(job.committed)
		{
			// The replacement machine cold-boots from the restored memory:
			// re-arm the boot gate and drop everything aimed at the old one.
			inst->framesSinceCreate = 0;
			inst->panelRows.reset();
			inst->pendingPanelReleases.clear();
			inst->pendingMidiIn.clear();
			inst->activeNotes.clear();
			inst->currentTrack = 0;
			const bool shadowMatches = job.shadow.size() == size_t(kNumTracks) * kNumParams;
			for(int t = 0; t < kNumTracks; ++t)
				for(size_t i = 0; i < kNumParams; ++i)
					inst->paramValues[t][kParams[i].key] =
						shadowMatches ? job.shadow[size_t(t) * kNumParams + i] : kParams[i].defaultV;
			inst->presetExtra.swap(job.extra);
			++inst->presetLoads;
		}
		job.stage.store(PresetJob::Finished, std::memory_order_release);
		return false;
	}
}

/* ---- plugin_api_v2 contract -- copied verbatim from
 * push-hack-xenia/src/bridge.c so the layout matches byte-for-byte. That
 * file is the authority; if it ever changes, this must change with it,
 * not the other way around. ---- */

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
	void *mm_create_instance(const char *module_dir, const char * /*json_defaults*/)
	{
		auto *inst = new MmInstance();

		try
		{
			std::string romDir = std::string(module_dir) + "/roms";

			synthLib::DeviceCreateParams params;
			params.preferredSamplerate = static_cast<float>(g_hostSampleRate);
			params.hostSamplerate = static_cast<float>(g_hostSampleRate);
			params.homePath = std::string(module_dir) + "/";
			params.customData = md::deviceCustomData(md::MachineModel::Monomachine);

			// md::RomLoader::findROM ultimately calls synthLib::RomLoader's
			// no-path findFiles overload, which searches the process's own
			// search-path list (addSearchPath), not an arbitrary directory
			// argument — unlike xenia_plugin.cpp, which instead chdir()s
			// the whole process into its ROM directory before calling
			// xtLib's equivalent. chdir() is process-global state that
			// would affect any other plugin sharing this same host
			// process's cwd-relative ROM lookups; addSearchPath has no such
			// side effect, so it's used here instead. Scans for an 8MiB
			// .bin matching Monomachine's fingerprint (mdtypes.h's
			// g_mmOs132bFingerprint).
			synthLib::RomLoader::addSearchPath(romDir);
			const auto rom = md::RomLoader::findROM(md::MachineModel::Monomachine);
			if(!rom.isValid())
			{
				inst->lastError = "no valid Monomachine ROM found in " + romDir +
					" (expected an 8MiB .bin matching OS 1.32b)";
				inst->bootFailed = true;
				return inst;
			}
			params.romData = rom.data();
			params.romName = rom.getFilename();

			inst->device = new md::Device(params);
			if(!inst->device->isValid())
			{
				inst->lastError = "device failed to construct from ROM";
				inst->bootFailed = true;
				return inst;
			}

			for(int t = 0; t < kNumTracks; ++t)
				for(const auto &p : kParams)
					inst->paramValues[t][p.key] = p.defaultV;

			// Pre-sized so render_block never allocates; it grows these
			// only if the host ever asks for a bigger block.
			inst->left.resize(4096);
			inst->right.resize(4096);
			inst->pendingMidiIn.reserve(256);
			inst->midiOut.reserve(256);

			// Boot-select track 0 so currentTrack and the firmware's own
			// selected-track state start in agreement (selectTrack no-ops
			// when track == currentTrack, so this only actually taps the
			// Track1 button, cheaply establishing a known state rather
			// than assuming the ROM's own power-on default matches).
			selectTrack(inst, 0);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("create_instance exception: ") + e.what();
			inst->bootFailed = true;
		}

		return inst;
	}

	void mm_destroy_instance(void *instance)
	{
		delete static_cast<MmInstance *>(instance);
	}

	void mm_on_midi(void *instance, const uint8_t *msg, int len, int /*source*/)
	{
		auto *inst = static_cast<MmInstance *>(instance);
		if(inst->bootFailed || len < 1 || isBooting(inst))
			return;

		const uint8_t status = msg[0];
		const uint8_t hi = status & 0xF0;
		const uint8_t data1 = len > 1 ? msg[1] : 0;
		const uint8_t data2 = len > 2 ? msg[2] : 0;

		try
		{
			// MIDI realtime (Clock/Start/Continue/Stop, 0xF8-0xFF, single
			// byte, no channel) forwarded straight through so real Monomachine
			// firmware's own MIDI transport-sync can drive Play/Stop/reset --
			// confirmed this is a real, deliberately modeled hardware feature
			// (not guessed): mdhardware.cpp's pumpGeneralFront() computes
			// byteCount from the status byte itself (1 for status >= 0xf0,
			// never the 3 a stray d1/d2 might otherwise imply), and a
			// separate pumpRealtime() path lets these bytes cross ahead of
			// other MIDI traffic mid-stream -- exactly how real hardware
			// treats realtime bytes, not something this bridge added.
			// queueMidi always takes 3 bytes; d1/d2=0 here is safe precisely
			// because that same byteCount logic ignores them for a >=0xf0
			// status (only the status byte itself is ever put on the wire).
			if(status >= 0xF8)
			{
				queueMidi(inst, status, 0, 0);
				return;
			}

			// Every channel-voice message (Note On/Off, Poly/Channel
			// Pressure, Pitch Bend) from the host is "play the currently
			// selected track live" — remapped to that track's own MIDI
			// channel (baseChannel+currentTrack), exactly the channel the
			// real hardware's own documented MIDI implementation expects
			// per-track messages on. This mirrors xenia_plugin.cpp's
			// kWorkingChannel0Indexed remap, just computed per-instance
			// instead of a compile-time constant, since which track is
			// "selected" changes at runtime.
			if(hi < 0x80 || hi > 0xE0)
				return; // SysEx/system messages: not handled by this bridge

			const uint8_t channel = static_cast<uint8_t>(
				(inst->baseChannel + inst->currentTrack) & 0x0F);
			const uint8_t outStatus = static_cast<uint8_t>(hi | channel);

			if(hi == 0x90 && data2 > 0)
				trackNoteOn(inst, channel, data1);
			else if(hi == 0x80 || (hi == 0x90 && data2 == 0))
				trackNoteOff(inst, channel, data1);

			queueMidi(inst, outStatus, data1, data2);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("on_midi exception: ") + e.what();
		}
	}

	// mm_set_param dispatches on key: a name in kParams is a per-track
	// synth parameter (MIDI CC automation, see this file's header
	// comment); everything else is a synthetic control this bridge
	// defines for the Go host's own use — none of these reach on_midi at
	// all, since panel buttons/trig keys/track select have no MIDI
	// equivalent on real hardware.
	void mm_set_param(void *instance, const char *key, const char *val)
	{
		auto *inst = static_cast<MmInstance *>(instance);
		if(inst->bootFailed || !key || !val)
			return;

		try
		{
			if(strcmp(key, "base_channel") == 0)
			{
				// Shadow-only bridge config, never touches the device --
				// exempt from isBooting() below so a startup-time call (see
				// main.go's setBaseChannelOnPlugin, fired once at launch,
				// well inside the boot window) still lands.
				int v = atoi(val);
				inst->baseChannel = v < 0 ? 0 : (v > 15 ? 15 : v);
				return;
			}
			if(strcmp(key, "preset_load") == 0)
			{
				// val is an absolute path. Allowed during boot: the restore
				// replaces the machine wholesale anyway.
				startLoad(inst, val);
				return;
			}
			// A load in flight is about to replace the machine: anything
			// sent now would land on the old one and be lost, silently
			// diverging from the host's shadow (see "ready" in get_param).
			if(isBooting(inst) || inst->preset.loadInFlight())
				return; // see MmInstance::framesSinceCreate's doc comment
			if(strcmp(key, "preset_save") == 0)
			{
				// val is "path\nextra" -- extra is opaque host data stored
				// in the same file (push-hack-mm: SEQ step shadow JSON).
				std::string s(val);
				const auto nl = s.find('\n');
				startSave(inst, s.substr(0, nl), nl == std::string::npos ? "" : s.substr(nl + 1));
				return;
			}
			if(strcmp(key, "track") == 0)
			{
				selectTrack(inst, atoi(val));
				return;
			}
			if(strcmp(key, "toggle_step") == 0)
			{
				// val is "track,step" — the pad-grid host's one call per
				// pad press (see this file's header comment on why the
				// select+tap sequence is atomic here rather than issued
				// as 2 separate set_param calls from Go).
				int track = 0, step = 0;
				if(sscanf(val, "%d,%d", &track, &step) == 2)
					toggleStep(inst, track, step);
				return;
			}
			if(strcmp(key, "toggle_mute") == 0)
			{
				// val is the track index — SEQ page's mute-row pad press
				// (see toggleMute's doc; does not change currentTrack).
				toggleMute(inst, atoi(val));
				return;
			}
			if(strcmp(key, "panel_track") == 0)
			{
				selectTrack(inst, atoi(val));
				return;
			}
			if(strcmp(key, "panel_button") == 0)
			{
				// val is "Name,down" or "Name,up" — Name is one of
				// panelControlNames()'s keys.
				std::string s(val);
				auto comma = s.find(',');
				if(comma == std::string::npos)
					return;
				std::string name = s.substr(0, comma);
				std::string dir = s.substr(comma + 1);
				auto &table = panelControlNames();
				auto it = table.find(name);
				if(it == table.end())
					return;
				if(dir == "down")
					pressControl(inst, it->second);
				else
					releaseControl(inst, it->second);
				return;
			}

			const ParamDef *def = findParam(key);
			if(!def)
				return; // unknown key -- no-op, matches xenia_plugin.cpp

			const int raw = atoi(val);
			const uint8_t v = static_cast<uint8_t>(
				raw < def->minV ? def->minV : (raw > def->maxV ? def->maxV : raw));
			inst->paramValues[inst->currentTrack][key] = v;

			md::automation::ParameterChange change{
				def->page, static_cast<uint8_t>(inst->currentTrack), def->index, v};
			auto cc = md::automation::encodeParameterChange(
				md::MachineModel::Monomachine, change,
				static_cast<uint8_t>(inst->baseChannel));
			if(cc)
				queueMidi3(inst, *cc);
		}
		catch(const std::exception &e)
		{
			inst->lastError = std::string("set_param exception: ") + e.what();
		}
	}

	int writeStr(char *buf, int buf_len, const std::string &v)
	{
		const int n = static_cast<int>(v.size());
		if(n >= buf_len) return -1;
		memcpy(buf, v.c_str(), static_cast<size_t>(n) + 1);
		return n;
	}

	int mm_get_param(void *instance, const char *key, char *buf, int buf_len)
	{
		auto *inst = static_cast<MmInstance *>(instance);

		if(strcmp(key, "engine_name") == 0) return writeStr(buf, buf_len, "Monomachine (Elektron SFX-60/6)");
		if(strcmp(key, "engine") == 0) return writeStr(buf, buf_len, "0");
		// "ready": "0" while the emulated firmware is still in its power-on
		// boot sequence -- see MmInstance::framesSinceCreate's doc comment.
		// MIDI/panel input is silently no-op'd by this bridge until this
		// flips to "1"; the Go host should poll it and hold off (or show a
		// "booting" status) rather than let pad presses appear to do
		// nothing with no explanation.
		// Also "0" while a preset load is in flight, so the host holds its
		// own shadow still until the replacement machine is up.
		if(strcmp(key, "ready") == 0)
			return writeStr(buf, buf_len, isBooting(inst) || inst->preset.loadInFlight() ? "0" : "1");
		if(strcmp(key, "preset_status") == 0) return writeStr(buf, buf_len, inst->preset.getStatus());
		if(strcmp(key, "preset_loads") == 0) return writeStr(buf, buf_len, std::to_string(inst->presetLoads));
		if(strcmp(key, "preset_extra") == 0) return writeStr(buf, buf_len, inst->presetExtra);
		if(strcmp(key, "track") == 0) return writeStr(buf, buf_len, std::to_string(inst->currentTrack));
		if(strcmp(key, "active_voices") == 0) return writeStr(buf, buf_len, std::to_string(inst->activeNotes.size()));

		if(strcmp(key, "chain_params") == 0)
		{
			// See xenia_plugin.cpp's own comment on why this must be a
			// JSON array of {key,name,type,min,max,options} objects, not
			// bare strings — push-hack's params.go parses it the same way
			// for every plugin. "track" is included so the host's generic
			// addSlot() mechanism (params.go) can create a slot for it
			// exactly like Xenia's "preset"/"octave_transpose", even
			// though it lives outside any paramPages grid page.
			std::string json = "[";
			auto appendEntry = [&](const char *k, const char *name, int lo, int hi)
			{
				if(json.size() > 1) json += ",";
				json += "{\"key\":\"" + std::string(k) + "\",\"name\":\"" + name +
					"\",\"type\":\"int\",\"min\":" + std::to_string(lo) +
					",\"max\":" + std::to_string(hi) + ",\"options\":[]}";
			};
			for(const auto &p : kParams)
				appendEntry(p.key, p.name, p.minV, p.maxV);
			appendEntry("track", "Track", 0, kNumTracks - 1);
			json += "]";
			return writeStr(buf, buf_len, json);
		}

		if(strcmp(key, "state") == 0)
		{
			// Current track's own shadow values — re-read by the Go host
			// right after instance creation and again every time the
			// selected track changes (params.go's syncFromPluginState),
			// so every slider shows that track's last-set values instead
			// of leaking the previously selected track's numbers.
			std::string json = "{";
			bool first = true;
			for(const auto &kv : inst->paramValues[inst->currentTrack])
			{
				if(!first) json += ",";
				first = false;
				json += "\"" + kv.first + "\":" + std::to_string(kv.second);
			}
			if(!first) json += ",";
			json += "\"track\":" + std::to_string(inst->currentTrack);
			json += "}";
			return writeStr(buf, buf_len, json);
		}

		if(strcmp(key, "panel_state") == 0)
		{
			// Live readback for the pad-grid LED sync (leds.go) and the
			// web UI's LCD visualizer — see mdLib/mdfrontpanel.h's
			// FrontPanel doc. Polled, not pushed: this bridge has no
			// callback mechanism into Go, so the host asks for a fresh
			// snapshot on its own display tick (~10fps on-screen, faster
			// for the web UI's SSE stream).
			if(inst->bootFailed)
				return writeStr(buf, buf_len, "{}");
			md::FrontPanel panel = inst->device->getFrontPanelSnapshot();

			std::string json = "{\"track\":" + std::to_string(inst->currentTrack) + ",\"steps\":[";
			for(int i = 0; i < kNumSteps; ++i)
			{
				if(i) json += ",";
				// 0=off,1=green,2=red,3=yellow — matches
				// FrontPanel::LedColor's own enumerator order.
				json += std::to_string(static_cast<int>(panel.getMonomachineStepLedColor(static_cast<uint32_t>(i))));
			}
			// NOT included here: a "playing"/"recording" transport flag.
			// mdLib/mdfrontpanel.h's StatusLed/ModeLed enums have no
			// clearly-transport-meaning bit to read one from —
			// StatusLed::Pattern reads as "Pattern mode is active" (vs
			// Song mode), not "is playing," and ModeLed::Record's own doc
			// comment calls it "grid-edit," not transport record-arm.
			// Rather than assert a semantics this bridge can't confirm,
			// the Go host tracks play/record as its own optimistic UI
			// shadow instead (see push-hack-mm/src/seq.go) — flagged
			// there as unverified against real hardware, same posture as
			// the other open assumptions in this file's header comment.
			json += ",\"modeLed\":" + std::to_string(panel.getLedBankRaw(md::FrontPanel::LedBank::Mode));

			// LCD: 128x64, 1 bit/pixel, row-major, MSB-first per byte —
			// 1024 bytes total, base64'd so it fits a plain JSON string
			// field. The web UI's canvas decoder (ui/index.html) expects
			// exactly this layout.
			std::vector<uint8_t> bits((128u * 64u + 7) / 8, 0);
			for(uint32_t y = 0; y < md::FrontPanel::g_lcdHeight; ++y)
				for(uint32_t x = 0; x < md::FrontPanel::g_lcdWidth; ++x)
					if(panel.getLcdPixel(x, y))
					{
						size_t bitIdx = y * md::FrontPanel::g_lcdWidth + x;
						bits[bitIdx / 8] |= static_cast<uint8_t>(0x80u >> (bitIdx % 8));
					}
			json += ",\"lcd\":\"" + base64Encode(bits.data(), bits.size()) + "\"}";
			return writeStr(buf, buf_len, json);
		}

		if(const ParamDef *def = findParam(key))
		{
			auto &values = inst->paramValues[inst->currentTrack];
			auto it = values.find(key);
			const uint8_t v = it != values.end() ? it->second : def->defaultV;
			return writeStr(buf, buf_len, std::to_string(v));
		}
		return -1;
	}

	int mm_get_error(void *instance, char *buf, int buf_len)
	{
		auto *inst = static_cast<MmInstance *>(instance);
		if(inst->lastError.empty()) return 0;
		return writeStr(buf, buf_len, inst->lastError);
	}

	void mm_render_block(void *instance, int16_t *out_interleaved_lr, int frames)
	{
		auto *inst = static_cast<MmInstance *>(instance);
		if(inst->bootFailed || frames <= 0)
		{
			if(frames > 0) memset(out_interleaved_lr, 0, static_cast<size_t>(frames) * 2 * sizeof(int16_t));
			return;
		}
		const bool fadeOut = servicePresetLoad(inst);
		drainPendingPanelReleases(inst);
		inst->framesSinceCreate += static_cast<uint64_t>(frames);

		try
		{
			auto &left = inst->left;
			auto &right = inst->right;
			if(left.size() < static_cast<size_t>(frames))
			{
				left.resize(static_cast<size_t>(frames));
				right.resize(static_cast<size_t>(frames));
			}
			std::array<float *, 12> outs{};
			outs[0] = left.data();
			outs[1] = right.data();
			// channels 2-11: individual-track outs (per getChannelCountOut,
			// mdLib reports 6) that this bridge doesn't route anywhere —
			// left null, matching how a multi-out VST3 host would leave
			// unused output buses unbound. The standalone/loopback path
			// this bridge exists for only ever wants the main stereo mix
			// (channels 0/1), same as this project's README documents for
			// its own standalone apps.
			std::array<const float *, 4> ins{};

			synthLib::TAudioOutputs typedOuts;
			synthLib::TAudioInputs typedIns;
			for(size_t i = 0; i < typedOuts.size(); ++i) typedOuts[i] = outs[i];
			for(size_t i = 0; i < typedIns.size(); ++i) typedIns[i] = ins[i];

			// midiOut discarded: device->host MIDI (e.g. clock) not consumed here
			inst->midiOut.clear();
			inst->device->process(typedIns, typedOuts, static_cast<size_t>(frames),
				inst->pendingMidiIn, inst->midiOut);
			inst->pendingMidiIn.clear();

			for(int i = 0; i < frames; ++i)
			{
				if(fadeOut)
				{
					const float g = 1.0f - static_cast<float>(i + 1) / static_cast<float>(frames);
					left[static_cast<size_t>(i)] *= g;
					right[static_cast<size_t>(i)] *= g;
				}
				auto clamp16 = [](float f) -> int16_t
				{
					const float scaled = f * kOutputHeadroom * 32767.0f;
					if(scaled > 32767.0f) return 32767;
					if(scaled < -32768.0f) return -32768;
					return static_cast<int16_t>(scaled);
				};
				out_interleaved_lr[i * 2]     = clamp16(left[static_cast<size_t>(i)]);
				out_interleaved_lr[i * 2 + 1] = clamp16(right[static_cast<size_t>(i)]);
			}

			tickNoteWatchdog(inst, static_cast<size_t>(frames));
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
		mm_create_instance,
		mm_destroy_instance,
		mm_on_midi,
		mm_set_param,
		mm_get_param,
		mm_get_error,
		mm_render_block,
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
