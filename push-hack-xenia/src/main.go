// push-braids — a Push3 standalone host proving the audio+MIDI+DSP
// chain end to end: reads pad/button MIDI straight off Push3's own ALSA
// sequencer port (the same port push-manager/automation/keyboard-visualizer
// already subscribe to, via the shared core/alsaseq package), feeds Note
// On/Off into a Move Anything plugin_api_v2 DSP module (Braids, loaded via
// cgo/dlopen — see bridge.c), and writes the rendered audio into the
// "Push Hack Virtual Audio" ALSA Loopback card built in
// hacks/push-audio-loopback.
//
// Catalog-installed hacks only ever get "-config <hack.json path>" as an
// argument (see hacks/push-catalog/push-catalog.sh's install_service), so
// this binary is fully config/file driven: no CLI flags for the PCM
// device, channels, rate, or buffer size. Channels/rate/period/buffer are
// negotiated live from whatever Live's own audio track actually opened
// (see hwparams/), not set by a human.
package main

/*
#cgo LDFLAGS: -lasound -ldl -lm
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/binary"
	"flag"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/federico-pepe/ableton-push-hack/core/alsapcm"
	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/hackcfg"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	cardID                = "PHVAudio"
	defaultPushManagerURL = "http://localhost:7701"
	// defaultWebPort matches hack.json's "port" field — kept in sync
	// manually since hackcfg.Load only uses this as a fallback for a
	// missing/unparsable field, not the normal path.
	defaultWebPort = 7707

	// How often the supervisor re-checks card presence / hw_params once
	// a session is already running — catches Live restarting with
	// different params, or the loopback card disappearing.
	steadyPollInterval = 2 * time.Second
	// Backoff while waiting for a dependency (card not present, or
	// present but not yet opened by Live) — deliberately short: both
	// conditions are expected to clear within a few seconds of normal
	// operation, not the kind of thing that benefits from a long wait.
	waitPollInterval = time.Second
)

// defaultParams sets Xenia's on-screen state to match the ROM's own boot
// default (the "Play Sound" patch, program 0) with cutoff/resonance left
// mid-range/low — a moderate starting timbre, not self-oscillating, safe
// to hear the moment a pad is pressed. Real notes from Push3's pads reach
// the device via on_midi independent of these.
var defaultParams = [][2]string{
	{"program", "0"},
	{"cutoff", "100"},
	{"resonance", "0"},
}

// midiHandler implements alsaseq.Handler, translating Push3's pad/button
// events into raw MIDI bytes for the DSP plugin's on_midi. Only note
// events matter for a sound-generator module like Braids; everything
// else (CC, aftertouch, clock) is ignored for this first pass.
//
// Fixed() runs on the ALSA read-loop goroutine; it must NOT call into the
// plugin directly. Braids' C++ instance state (voice envelopes,
// oscillators) is not thread-safe, and the render loop calls into the
// same instance from its own dedicated goroutine — two goroutines hitting
// the same C++ object with no synchronization is a real data race, not a
// hypothetical one. So Fixed() only parses and forwards raw bytes over a
// channel; every bridge_plugin_* call happens on the render goroutine,
// which drains this channel first.
type midiHandler struct {
	out     chan<- [3]byte
	ctl     chan<- controlEvent
	pmURL   string
	params  *paramState
	io      *ioState
	astatus *audioStatus
	rt      *sharedConfig
	level   *levelMeter
}

// ctlKind is a controlEvent's kind — see controlEvent's doc.
type ctlKind int

const (
	ctlEncoder      ctlKind = iota // idx 0-7, delta = tick count
	ctlPageJump                    // idx = page index (top-screen button 1-4 pressed)
	ctlBottomPress                 // idx = button index 0-7 (bottom-screen button pressed)
	ctlSetParam                    // key/val = absolute param write (see webserver.go) — not from Push hardware
	ctlBankFlip                    // idx = target bank (0 or 1) — D-Pad Left/Right, see setBank in params.go
	ctlMasterVolume                // delta = tick count — Push3's dedicated hardware Volume encoder
)

// controlEvent is a CC-derived UI action decoded on the ALSA read-loop
// goroutine and applied on the render goroutine (audiosession.go's
// drainCtl), the same split as note messages and for the same reason:
// every bridge_plugin_* call must happen from the one goroutine that owns
// the plugin instance (see midiHandler's doc comment above).
//
// key/val are only set for ctlSetParam, the web UI's absolute-value write
// path (see webserver.go's handleSetParam) — every other kind is Push
// hardware's relative-delta encoder feel and uses idx/delta instead.
type controlEvent struct {
	kind  ctlKind
	idx   int
	delta int
	key   string
	val   float64
}

func (h *midiHandler) Fixed(evType uint8, src alsaseq.Addr, data []byte) {
	fromPush3 := src.Client == alsaseq.Push3ClientDefault

	if evType == alsaseq.EvController {
		// Only Push3's own default port drives the on-screen controls —
		// merging everything onto one port means CC could otherwise arrive
		// from any external client connected to it too.
		if !fromPush3 || src.Port != alsaseq.Push3PortDefault {
			return
		}
		// Push's own control-surface CCs (encoders, D-Pad, screen buttons)
		// are always channel 0. Every other channel carries per-pad MPE
		// expression from the pad grid — Push 3 assigns each held pad its
		// own channel and streams Channel Pressure plus CC 74 (MPE
		// "timbre"/Y-axis) on it — and CC 74 falls inside CCEncoder1-8's
		// range (71-78). Without this channel check, holding a pad reads
		// as encoder 4 spinning wildly (DecodeRel misreading an absolute
		// 0-127 MPE value as a relative delta), slamming whatever param
		// sits there to one extreme in a single block. This was the
		// reported "velocity jumps from 0% to 100%" bug — a pad's MPE
		// stream, not the pad's actual Note On velocity, was hitting the
		// encoder decoder.
		if data[0]&0x0F != 0 {
			return
		}
		cc := uint8(binary.LittleEndian.Uint32(data[4:]) & 0x7F)
		val := uint8(binary.LittleEndian.Uint32(data[8:]) & 0x7F)

		if cc == ccShift || cc == ccDevice {
			onChordCC(cc, val, h.pmURL, h.params, h.io, h.astatus, h.level)
			return
		}

		// D-Pad Left/Right flips between the 2 param-page banks (11 real
		// Microwave XT param groups don't fit Push's 8 top-screen buttons
		// -- see params.go's bankPageNames doc). D-Pad Up/Down and Select
		// are still unused -- top/bottom screen buttons replace those
		// entirely (page nav + per-page actions), and their LEDs go dark
		// to match (see leds.go). Not matching any case below is a silent
		// no-op, same as any other CC this host doesn't care about.
		var ev controlEvent
		switch {
		case cc >= push3.CCEncoder1 && cc <= push3.CCEncoder8:
			ev = controlEvent{kind: ctlEncoder, idx: int(cc) - push3.CCEncoder1, delta: push3.DecodeRel(val)}
		case cc >= push3.CCScreenTop1 && int(cc)-int(push3.CCScreenTop1) < len(pageNames) && val == 127:
			ev = controlEvent{kind: ctlPageJump, idx: int(cc) - push3.CCScreenTop1}
		case cc >= push3.CCScreenBot1 && cc <= push3.CCScreenBot8 && val == 127:
			ev = controlEvent{kind: ctlBottomPress, idx: int(cc) - push3.CCScreenBot1}
		case cc == push3.CCDPadLeft && val == 127:
			ev = controlEvent{kind: ctlBankFlip, idx: 0}
		case cc == push3.CCDPadRight && val == 127:
			ev = controlEvent{kind: ctlBankFlip, idx: 1}
		case cc == push3.CCVolume:
			ev = controlEvent{kind: ctlMasterVolume, delta: push3.DecodeRel(val)}
		default:
			return
		}
		select {
		case h.ctl <- ev:
		default:
			log.Printf("control channel full, dropped CC event cc=%d val=%d", cc, val)
		}
		return
	}

	if evType == alsaseq.EvPitchBend {
		// Push3's touch strip reports its position as pitch bend on
		// channel 1 (push3.NoteTouchStrip's doc) — only Push3 itself
		// sends this on the port we watch. value is signed, -8192..8191.
		if !fromPush3 {
			return
		}
		value := int32(binary.LittleEndian.Uint32(data[8:]))
		if isShiftHeld() {
			// Shift+strip repurposes it as mod_wheel instead of pitch
			// bend — same physical gesture, different destination.
			v127 := uint8((value + 8192) * 127 / 16383)
			select {
			case h.ctl <- controlEvent{kind: ctlSetParam, key: "mod_wheel", val: float64(v127)}:
			default:
				log.Printf("control channel full, dropped shift+strip mod_wheel event")
			}
			return
		}
		channel := data[0] & 0x0F
		bend := uint32(value + 8192)
		msg := [3]byte{0xE0 | channel, uint8(bend & 0x7F), uint8((bend >> 7) & 0x7F)}
		select {
		case h.out <- msg:
		default:
			log.Printf("MIDI channel full, dropped pitch bend event")
		}
		return
	}

	var status byte
	switch evType {
	case alsaseq.EvNoteOn:
		status = 0x90
	case alsaseq.EvNoteOff:
		status = 0x80
	default:
		return
	}

	if fromPush3 {
		// Only count Push3's own notes while the I/O picker still points
		// at the port they arrived on — switching it elsewhere quiets
		// Push3's own pads instead of double-triggering both sources.
		curClient, curPort := h.rt.getMIDI()
		if src.Client != curClient || src.Port != curPort {
			return
		}
		// Push3's own touch-sensitive controls (encoder touch = notes 0-7,
		// D-Pad center touch = note 13, etc. — docs/push3-button-map.md)
		// send real Note On/Off outside the pad grid's 36-99 range —
		// reject those, only the pad grid should trigger a voice. External
		// gear isn't Push3 hardware, so it isn't range-limited this way.
		if data[1] < 36 || data[1] > 99 {
			return
		}
	} else if want := h.rt.getRecvChannel(); want >= 0 && int(data[0]&0x0F) != want {
		// Non-Push3 sources (external gear, Live MIDI clips) only trigger
		// a voice on the configured receive channel, if one is set —
		// otherwise every track routed to "Xenia MIDI In" sounds at once.
		// Push3's own pads are never filtered this way: the firmware
		// assigns each held pad its own MPE channel, so a fixed channel
		// filter would silence most of the pad grid.
		return
	}

	channel := data[0] & 0x0F
	note := data[1]
	velocity := data[2]
	msg := [3]byte{status | channel, note, velocity}

	select {
	case h.out <- msg:
	default:
		log.Printf("MIDI channel full, dropped event type=%d note=%d vel=%d", evType, note, velocity)
	}
}

func (h *midiHandler) VarLen(evType uint8, src alsaseq.Addr, payload []byte) {
	// SysEx etc. — not relevant to a sound-generator module, ignored.
}

func main() {
	// A catalog install only ever respawns this process by restarting the
	// init.d service, which does not happen on its own if the process
	// merely crashes. Re-exec as a supervised child so a crash in the
	// cgo/dlopen/ALSA code below (the real crash-prone surface) gets
	// retried without needing a human or a service restart. The parent
	// here does no cgo of its own, so it is extremely unlikely to crash
	// itself.
	if os.Getenv("PBH_SUPERVISED") != "1" {
		runSupervisor()
		return
	}
	runSupervised()
}

func runSupervisor() {
	// The init.d service only ever signals this top-level PID — it has no
	// idea a child process exists. Without forwarding the signal, "stop"
	// kills only this parent and leaves the child running as an orphan:
	// the service looks stopped but audio/MIDI keep running, and the next
	// deploy's scp fails with ETXTBSY because the orphan still has the
	// binary open for execution. Forward SIGINT/SIGTERM to the child and
	// wait for it to actually exit before this process does too.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		cmd := exec.Command(os.Args[0], os.Args[1:]...)
		cmd.Env = append(os.Environ(), "PBH_SUPERVISED=1")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Printf("starting supervised child: %v — retrying in %v", err, backoff)
			time.Sleep(backoff)
			continue
		}

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		var err error
		var ran time.Duration
		start := time.Now()
		select {
		case sig := <-sigCh:
			log.Printf("received %v, forwarding to child and exiting", sig)
			_ = cmd.Process.Signal(sig)
			<-done
			return
		case err = <-done:
			ran = time.Since(start)
		}
		log.Printf("supervised child exited after %v: %v", ran, err)

		// A child that ran a good while before dying gets a fast retry —
		// treat it as an isolated crash, not a boot loop.
		if ran > 30*time.Second {
			backoff = time.Second
		}
		log.Printf("respawning in %v", backoff)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func runSupervised() {
	configPath := flag.String("config", "hack.json", "path to hack.json config file")
	flag.Parse()
	hackDir, err := filepath.Abs(filepath.Dir(*configPath))
	if err != nil {
		log.Fatalf("resolving hack dir: %v", err)
	}

	// Defers all /dev/snd access until system uptime clears the cold-boot
	// USB-A enumeration window — see core/alsaseq/bootsettle.go. A
	// catalog-installed hack starts at boot, squarely inside that window.
	alsaseq.WaitForBootSettle()

	cfg, err := loadConfig(hackDir)
	if err != nil {
		log.Fatalf("loading %s: %v", configFileName, err)
	}
	pmURL := cfg.PushManagerURL
	if pmURL == "" {
		pmURL = defaultPushManagerURL
	}

	// The audio render goroutine itself is allocation-free (all buffers
	// pre-allocated, no per-iteration heap traffic) and stays that way —
	// but the display loop legitimately allocates a fresh PNG frame on
	// every redraw (runDisplayLoop, for the Volume fader's live meter).
	// Disabling GC process-wide used to be safe when nothing allocated at
	// all; now it guarantees an OOM crash the longer the UI stays open
	// (confirmed on hardware). Leave GC at its default — a concurrent GC's
	// brief pauses run on their own goroutine, not the SCHED_FIFO render
	// thread, so this doesn't reintroduce the glitch the disable above was
	// originally guarding against.

	dspPath := filepath.Join(hackDir, "dsp.so")
	moduleDir := filepath.Join(hackDir, "module")

	log.Printf("loading DSP plugin: %s (module dir: %s)", dspPath, moduleDir)
	cSo := C.CString(dspPath)
	defer C.free(unsafe.Pointer(cSo))
	cDir := C.CString(moduleDir)
	defer C.free(unsafe.Pointer(cDir))

	// sample_rate/frames_per_block here only size the plugin's own
	// internal buffers at load time — the actual PCM session's real rate
	// and period (negotiated live, per device) are supplied separately to
	// each bridge_pcm_open call in audiosession.go.
	const pluginInitRate = 44100
	const pluginInitBlock = 128
	plugin := C.bridge_plugin_load(cSo, cDir, C.int(pluginInitRate), C.int(pluginInitBlock))
	if plugin == nil {
		log.Fatalf("bridge_plugin_load failed: %s", C.GoString(C.bridge_last_error()))
	}
	defer C.bridge_plugin_unload(plugin)
	log.Printf("plugin loaded and instance created")

	for _, kv := range defaultParams {
		k, v := C.CString(kv[0]), C.CString(kv[1])
		C.bridge_plugin_set_param(plugin, k, v)
		C.free(unsafe.Pointer(k))
		C.free(unsafe.Pointer(v))
	}

	metas, err := fetchChainParams(plugin)
	if err != nil {
		log.Fatalf("fetchChainParams: %v", err)
	}
	// "preset" isn't part of chain_params -- it's this host's own staged
	// browse-list UI over the real "program" key (see fetchPresetMeta),
	// folded in here so it slots into paramPages like any other param.
	metas = append(metas, fetchPresetMeta())
	params := newParamState(metas)
	// v2_create_instance auto-loads preset 0 (if any presets exist) after
	// the defaultParams loop above — resync so the very first frame shows
	// real values, not defaultParams's guesses, for every param.
	params.syncFromPluginState(plugin)

	// rt is persistedConfig's live counterpart: watchBraidsPort/watchHWParams
	// act on it, and the SETTINGS page (Shift+Device, top-screen button 4)
	// writes to it when the user picks a different port or device — no
	// process restart needed, and it's saved back to xenia-config.json
	// right after (see iopage.go's commitMIDI/commitDevice/commitChannel).
	rt := newSharedConfig(cfg)
	io := newIOState(hackDir, rt)
	astatus := &audioStatus{msg: msgWaitingForCard} // starting guess till watchHWParams' 1st check
	level := &levelMeter{}
	diag := &diagStats{} // CPU%/active-voice diagnostics -- see audiosession.go

	go runDependencyWatcher(pmURL)
	go runDisplayLoop(pmURL, params, io, astatus, level)

	midiCh := make(chan [3]byte, 256)
	ctlCh := make(chan controlEvent, 64)
	ctlChWrite = ctlCh // see audiosession.go's scheduleProgramCommit
	handler := &midiHandler{out: midiCh, ctl: ctlCh, pmURL: pmURL, params: params, io: io, astatus: astatus, rt: rt, level: level}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	shutdown := make(chan struct{})
	go func() {
		sig := <-sigCh
		log.Printf("signal received (%v), stopping...", sig)
		close(shutdown)
	}()

	// web_ui's port, per hack.json — a catalog-installed hack only ever
	// gets "-config <hack.json path>" as an argument (see this file's doc
	// comment), so hackcfg.Load reads the same file *configPath already
	// points at. A missing/unparsable port falls back to the value baked
	// into hack.json (defaultWebPort) rather than failing startup — the
	// web UI is optional, unlike the audio/MIDI path.
	hcfg, err := hackcfg.Load(*configPath, defaultWebPort)
	if err != nil {
		log.Printf("loading %s for web UI port: %v (defaulting to %d)", *configPath, err, defaultWebPort)
		hcfg.Port = defaultWebPort
	}
	go runWebServer(hcfg.Port, hcfg.Version, params, io, astatus, diag, ctlCh, shutdown)

	// One port, see midisession.go doc: pinned Push3 control surface +
	// notes, picker-retargetable notes, and always open for external gear.
	go watchBraidsPort(rt, handler, shutdown)

	// The audio session itself — PCM open/close, channels/rate/period —
	// is fully owned by this supervisor loop, which blocks until shutdown
	// fires. It negotiates live off Live's own hw_params instead of a
	// value someone guessed and hardcoded, and reopens whenever those
	// params (or the user's chosen PCM device, via rt) change. See
	// audiosession.go.
	watchHWParams(cardID, rt, plugin, midiCh, ctlCh, params, io, astatus, level, diag, shutdown)

	// Best-effort: leaving push-manager's MIDI intercept or display
	// takeover stuck on after this process exits would silently block pad
	// input to Live / freeze the screen with no process left to blame.
	shutdownUI(pmURL)
	log.Printf("stopped")
}

func cardPresent(id string) bool {
	cards, err := alsapcm.EnumCards()
	if err != nil {
		return false
	}
	for _, c := range cards {
		if c.ID == id {
			return true
		}
	}
	return false
}
