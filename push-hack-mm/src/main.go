// push-mm — a Push3 standalone host for Elektron Monomachine emulation
// (gearmulator-md-mm's mdLib), adapted from push-hack-xenia's proven
// architecture: reads pad/button MIDI straight off Push3's own ALSA
// sequencer port, feeds it into a Move Anything plugin_api_v2 DSP module
// (mm-plugin, loaded via cgo/dlopen — see bridge.c, unmodified from
// push-hack-xenia's own copy), and writes rendered audio into
// push-audio-loopback's virtual card.
//
// The one real architectural addition over push-hack-xenia: Push's pad
// grid is dual-mode. Normally (any page except SEQ) a pad press plays the
// currently selected track live, exactly like Xenia's pads always played
// a note — the DSP plugin remaps the channel internally based on
// whichever track is selected (mm_plugin.cpp's on_midi), so this host
// never needs to know or rewrite the channel itself. While the on-screen
// UI is open AND showing the SEQ page, the SAME pads instead become a
// 6-track x 16-step grid sequencer (row = track, matching "access each
// voice" directly with pads) by simulating the real front panel's Track
// select + Trigger key presses (see mm_plugin.cpp's toggle_step) — not by
// reimplementing sequencing logic in Go, since mdLib runs the actual
// Monomachine firmware's own real sequencer.
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
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/federico-pepe/ableton-push-hack/core/alsapcm"
	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/hackcfg"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	cardID                = "Audio"
	defaultPushManagerURL = "http://localhost:7701"
	defaultWebPort        = 7708
	// defaultHubURL — push-hub's own well-known local port (see
	// docs/push-hub-proposal.md). Probed once at startup (chord.go's
	// probeHub) to decide whether this hack's own Shift+Device binding
	// should stand down in favor of hub-driven focus.
	defaultHubURL = "http://localhost:7709"

	steadyPollInterval = 2 * time.Second
	waitPollInterval   = time.Second
)

// ctlKind is a CC/pad-derived UI action decoded on the ALSA read-loop
// goroutine and applied on the render goroutine (audiosession.go's
// drainCtl) — see midiHandler's doc comment below for why that split
// exists.
type ctlKind int

const (
	ctlEncoder     ctlKind = iota // idx 0-7, delta = tick count
	ctlPageJump                   // idx = page index (top-screen button pressed)
	ctlBottomPress                // idx = button index 0-7 (bottom-screen button pressed)
	ctlSetParam                   // key/val = absolute param write (web UI)
	ctlBankFlip                   // idx = target bank (0 or 1)
	ctlTrackStep                  // delta = +1/-1 (D-Pad Up/Down)
	ctlToggleStep                 // idx = track, delta = step (SEQ page pad press)
	ctlMuteToggle                 // idx = track (SEQ page bottom-row pad press)
	ctlTransport                  // key = "play"|"stop"|"record" — web UI's transport buttons; works regardless of Push's current on-screen page
	ctlStepPage                   // idx = 0 (toggle) or 1 (explicit page, val = 0/1) — web UI's page flip
	ctlPresetSave                 // key = absolute preset path (webserver.go's presetPath)
	ctlPresetLoad                 // key = absolute preset path
	ctlBaseChannel                // val = absolute 0-indexed channel (0-15) — web UI's base-channel selector; SEQ page's own encoder 0 already sets this directly in audiosession.go, this is the same effect via an absolute value instead of a delta
)

type controlEvent struct {
	kind  ctlKind
	idx   int
	delta int
	key   string
	val   float64
}

// midiHandler implements alsaseq.Handler, translating Push3's pad/button
// events into raw MIDI bytes (PLAY mode) or controlEvents (SEQ mode, and
// every UI/track-select action) for the DSP plugin.
//
// Fixed() runs on the ALSA read-loop goroutine; it must NOT call into the
// plugin directly — every bridge_plugin_* call happens on the render
// goroutine (audiosession.go), which drains the channels this handler
// only ever sends into. Same discipline as push-hack-xenia/src/main.go,
// for the same reason (the DSP plugin instance is not thread-safe).
type midiHandler struct {
	out     chan<- [3]byte
	ctl     chan<- controlEvent
	pmURL   string
	params  *paramState
	io      *ioState
	astatus *audioStatus
	seq     *seqState
	rt      *sharedConfig
	level   *levelMeter
}

func (h *midiHandler) Fixed(evType uint8, src alsaseq.Addr, data []byte) {
	fromPush3 := src.Client == alsaseq.Push3ClientDefault

	// push-hub arbitrates which hack currently reads Push3's own pads/CCs
	// (docs/push-hub-proposal.md) — every fromPush3 branch below (control
	// surface AND pad notes) is a no-op while this hack isn't the focused
	// one. Defaults true (see display.go's focused var), so a hack run
	// without push-hub installed behaves exactly as before this existed.
	// External gear/Live (fromPush3==false) is never gated here.
	if fromPush3 && !isFocused() {
		return
	}

	if evType == alsaseq.EvController {
		if !fromPush3 || src.Port != alsaseq.Push3PortDefault {
			return
		}
		// See push-hack-xenia/src/main.go's doc on why this channel check
		// is load-bearing: a held pad's own MPE stream (Channel Pressure +
		// CC74) lands on a non-zero channel and falls inside CCEncoder1-8's
		// range — without this, a held pad reads as encoder 4 spinning.
		if data[0]&0x0F != 0 {
			return
		}
		cc := uint8(binary.LittleEndian.Uint32(data[4:]) & 0x7F)
		val := uint8(binary.LittleEndian.Uint32(data[8:]) & 0x7F)

		if cc == ccShift || cc == ccDevice {
			onChordCC(cc, val, h.pmURL, h.params, h.io, h.astatus, h.seq, h.level)
			return
		}

		var ev controlEvent
		switch {
		case cc >= push3.CCEncoder1 && cc <= push3.CCEncoder8:
			ev = controlEvent{kind: ctlEncoder, idx: int(cc) - push3.CCEncoder1, delta: push3.DecodeRel(val)}
		case cc >= push3.CCScreenTop1 && int(cc)-int(push3.CCScreenTop1) < len(pageNames) && val == 127:
			ev = controlEvent{kind: ctlPageJump, idx: int(cc) - push3.CCScreenTop1}
		case cc == push3.CCScreenBot8 && val == 127 && h.params.Page() == pageSettings:
			// EXIT, bottom-right on the SETTINGS page (see iopage.go's
			// render() / leds.go's pageBottomLit) -- same UI-only action as
			// the Shift+Device chord (onChordCC), so it's dispatched the
			// same way: directly here, never through ctlCh, since toggleUI
			// only talks to push-manager over HTTP and never touches the
			// DSP plugin (no render-goroutine restriction applies to it).
			// Suppressed when push-hub is present, same as chord.go's own
			// Shift+Device standdown -- otherwise this would flip uiOn/
			// focused locally without telling the hub, leaving its menu
			// showing this hack as still-focused after the screen's
			// already back to native Push/Live. Shift+Device (hub-owned)
			// is the only way back to the picker once hub is installed.
			if !hubPresent {
				go toggleUI(h.pmURL, h.params, h.io, h.astatus, h.seq, h.level)
			}
			return
		case cc == push3.CCScreenBot4 && val == 127 && h.params.Page() == pageSettings:
			// QUIT, SETTINGS page (see iopage.go's render()) -- dispatched
			// directly, same reasoning as EXIT just above: this is a
			// process-lifecycle action (selfcontrol.go), nothing to do
			// with the DSP plugin, so no render-goroutine restriction
			// applies and it doesn't need to go through ctlCh.
			go quitSelf()
			return
		case cc == push3.CCScreenBot6 && val == 127 && h.params.Page() == pageSettings:
			// RESTART, SETTINGS page -- see selfcontrol.go's restartSelf.
			go restartSelf()
			return
		case cc >= push3.CCScreenBot1 && cc <= push3.CCScreenBot8 && val == 127:
			ev = controlEvent{kind: ctlBottomPress, idx: int(cc) - push3.CCScreenBot1}
		case cc == push3.CCDPadLeft && val == 127:
			ev = controlEvent{kind: ctlBankFlip, idx: 0}
		case cc == push3.CCDPadRight && val == 127:
			ev = controlEvent{kind: ctlBankFlip, idx: 1}
		case cc == push3.CCDPadUp && val == 127:
			ev = controlEvent{kind: ctlTrackStep, delta: 1}
		case cc == push3.CCDPadDown && val == 127:
			ev = controlEvent{kind: ctlTrackStep, delta: -1}
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
		if !fromPush3 {
			return
		}
		value := int32(binary.LittleEndian.Uint32(data[8:]))
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

	if fromPush3 && push3.IsPadNote(data[1]) && uiIsOn() && h.params.Page() == pageSeq {
		// SEQ mode: this pad press is a step/mute toggle, not a note.
		// Note Off is a no-op here (a tap is press-and-release on the
		// SAME logical action, applied entirely on Note On, matching
		// mm_plugin.cpp's toggle_step, which itself presses+releases the
		// trig key in one call) — only forward Note On with velocity>0.
		if evType != alsaseq.EvNoteOn || data[2] == 0 {
			return
		}
		col, row := push3.PadCoord(data[1])
		switch {
		case row >= 1 && row <= mmNumTracks: // rows 1-6 (bottom-up): tracks 1-6's step grid, see leds.go's syncPadLEDs doc for why row 0 is the spare row, not a gap in the middle
			track := row - 1
			step := col + h.seq.StepPage()*mmStepsPerPage
			select {
			case h.ctl <- controlEvent{kind: ctlToggleStep, idx: track, delta: step}:
			default:
				log.Printf("control channel full, dropped step toggle")
			}
		case row == 7 && col < mmNumTracks: // top row: per-track mute
			select {
			case h.ctl <- controlEvent{kind: ctlMuteToggle, idx: col}:
			default:
				log.Printf("control channel full, dropped mute toggle")
			}
		}
		return
	}

	if fromPush3 {
		curClient, curPort := h.rt.getMIDI()
		if src.Client != curClient || src.Port != curPort {
			return
		}
		// Push3's own touch-sensitive controls send Note On/Off outside
		// the pad grid's 36-99 range — reject those here, same as Xenia.
		if data[1] < 36 || data[1] > 99 {
			return
		}
	} else if want := h.rt.getRecvChannel(); want >= 0 && int(data[0]&0x0F) != want {
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
	// SysEx etc. — not consumed by this host; mm-plugin's own on_midi
	// only forwards channel-voice messages (see mm_plugin.cpp).
}

func main() {
	if os.Getenv("PBH_SUPERVISED") != "1" {
		runSupervisor()
		return
	}
	runSupervised()
}

// runSupervisor re-execs as a supervised child so a crash in the
// cgo/dlopen/ALSA code below gets retried without a human or service
// restart — identical to push-hack-xenia/src/main.go's runSupervisor.
func runSupervisor() {
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

	alsaseq.WaitForBootSettle()

	cfg, err := loadConfig(hackDir)
	if err != nil {
		log.Fatalf("loading %s: %v", configFileName, err)
	}
	pmURL := cfg.PushManagerURL
	if pmURL == "" {
		pmURL = defaultPushManagerURL
	}

	// One-time probe (see chord.go's doc) — if push-hub is already up,
	// this hack's own Shift+Device binding stands down and focus becomes
	// entirely hub-driven via POST /api/focus (webserver.go). Not
	// re-checked live: installing push-hub after this hack is already
	// running needs a restart of this hack to take effect.
	hubPresent = probeHub(defaultHubURL)
	if hubPresent {
		log.Printf("push-hub detected at %s — local Shift+Device disabled, focus is hub-controlled", defaultHubURL)
		// focused defaults true (display.go's var block) for the
		// standalone-no-hub case, where nothing else would ever set it.
		// With a hub present, default the other way instead: this
		// process was likely just launched by the hub's own START button
		// (push-hub/src/focus.go's startDirect), and nothing has
		// explicitly focused it yet at this point -- defaulting true
		// meant a freshly-started, not-yet-focused hack was immediately
		// audible (audiosession.go's isFocused() gate) and would fight
		// the hub for the screen the instant it opened its own UI, part
		// of the reported "focus feature is still very buggy". POST
		// /api/focus {"on":true} (webserver.go's handleFocus) is the only
		// thing that should ever flip this back to true once a hub is
		// present.
		setFocused(false)
	}

	dspPath := filepath.Join(hackDir, "dsp.so")
	moduleDir := filepath.Join(hackDir, "module")

	log.Printf("loading DSP plugin: %s (module dir: %s)", dspPath, moduleDir)
	cSo := C.CString(dspPath)
	defer C.free(unsafe.Pointer(cSo))
	cDir := C.CString(moduleDir)
	defer C.free(unsafe.Pointer(cDir))

	const pluginInitRate = 44100
	const pluginInitBlock = 128
	plugin := C.bridge_plugin_load(cSo, cDir, C.int(pluginInitRate), C.int(pluginInitBlock))
	if plugin == nil {
		log.Fatalf("bridge_plugin_load failed: %s", C.GoString(C.bridge_last_error()))
	}
	defer C.bridge_plugin_unload(plugin)
	log.Printf("plugin loaded and instance created")

	setBaseChannelOnPlugin(plugin, cfg.BaseChannel)

	metas, err := fetchChainParams(plugin)
	if err != nil {
		log.Fatalf("fetchChainParams: %v", err)
	}
	params := newParamState(metas)
	params.syncFromPluginState(plugin)

	rt := newSharedConfig(cfg)
	io := newIOState(hackDir, rt)
	astatus := &audioStatus{msg: msgWaitingForCard}
	level := &levelMeter{}
	diag := &diagStats{}
	seq := newSeqState()

	go runDependencyWatcher(pmURL)
	go runDisplayLoop(pmURL, params, io, astatus, seq, level)

	midiCh := make(chan [3]byte, 256)
	ctlCh := make(chan controlEvent, 64)
	ctlChWrite = ctlCh
	handler := &midiHandler{out: midiCh, ctl: ctlCh, pmURL: pmURL, params: params, io: io, astatus: astatus, seq: seq, rt: rt, level: level}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	shutdown := make(chan struct{})
	go func() {
		sig := <-sigCh
		log.Printf("signal received (%v), stopping...", sig)
		close(shutdown)
	}()

	hcfg, err := hackcfg.Load(*configPath, defaultWebPort)
	if err != nil {
		log.Printf("loading %s for web UI port: %v (defaulting to %d)", *configPath, err, defaultWebPort)
		hcfg.Port = defaultWebPort
	}
	go runWebServer(hcfg.Port, hcfg.Version, pmURL, params, io, astatus, diag, seq, level, ctlCh, shutdown)

	go watchMMPort(rt, handler, shutdown)

	watchHWParams(cardID, rt, plugin, midiCh, ctlCh, params, io, astatus, seq, level, diag, shutdown)

	shutdownUI(pmURL)
	log.Printf("stopped")
}

// setBaseChannelOnPlugin pushes cfg's base channel to the plugin at
// startup, via audiosession.go's shared cSetParam helper.
func setBaseChannelOnPlugin(plugin *C.bridge_plugin_t, ch int) {
	cSetParam(plugin, "base_channel", strconv.Itoa(ch))
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
