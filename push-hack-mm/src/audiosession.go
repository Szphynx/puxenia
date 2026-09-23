package main

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"push-mm/hwparams"
)

// cSetParam is bridge_plugin_set_param's CString/free boilerplate, shared
// by every drainCtl case that sends a param to the plugin. Identical to
// push-hack-xenia/src/audiosession.go's own helper.
func cSetParam(plugin *C.bridge_plugin_t, key, val string) {
	k, v := C.CString(key), C.CString(val)
	C.bridge_plugin_set_param(plugin, k, v)
	C.free(unsafe.Pointer(k))
	C.free(unsafe.Pointer(v))
}

// ctlChWrite is a writable handle onto the same channel main.go passes
// everywhere else as receive-only (<-chan controlEvent) — kept for parity
// with push-hack-xenia's shape even though this host currently has no
// debounced-commit timer of its own (Monomachine has no Xenia-style
// slow-patch-load state machine to protect against a flood of quick
// writes — panel taps and CC automation are both simple per-message
// operations on real hardware).
var ctlChWrite chan<- controlEvent

type levelMeter struct{ bits atomic.Uint64 }

func (m *levelMeter) set(v float64) { m.bits.Store(math.Float64bits(v)) }
func (m *levelMeter) get() float64  { return math.Float64frombits(m.bits.Load()) }

type diagStats struct {
	cpuBits atomic.Uint64
	voices  atomic.Int32
	// deviceReady mirrors mm_plugin.cpp's get_param("ready"): false while
	// the emulated Monomachine firmware is still in its ~22s power-on boot
	// sequence, during which the plugin silently no-ops every MIDI/panel
	// event it receives (see mm_plugin.cpp's MmInstance::framesSinceCreate
	// doc comment). Zero value is false, which is correct: nothing has
	// polled the plugin yet at construction, and it genuinely isn't ready.
	deviceReady atomic.Bool
}

func (d *diagStats) setCPU(pct float64)  { d.cpuBits.Store(math.Float64bits(pct)) }
func (d *diagStats) getCPU() float64     { return math.Float64frombits(d.cpuBits.Load()) }
func (d *diagStats) setVoices(n int)     { d.voices.Store(int32(n)) }
func (d *diagStats) getVoices() int      { return int(d.voices.Load()) }
func (d *diagStats) setReady(ready bool) { d.deviceReady.Store(ready) }
func (d *diagStats) getReady() bool      { return d.deviceReady.Load() }

func selfCPUTicks() (uint64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return 0, fmt.Errorf("bad /proc/self/stat")
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("short /proc/self/stat")
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	return utime + stime, nil
}

func systemCPUTicks() (uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			break
		}
		var total uint64
		for i := 1; i < 8; i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			total += v
		}
		return total, nil
	}
	return 0, fmt.Errorf("no cpu line in /proc/stat")
}

// audioSession owns one open PCM handle and the goroutine rendering into
// it — identical shape to push-hack-xenia/src/audiosession.go's own type.
type audioSession struct {
	pcm      *C.bridge_pcm_t
	period   int
	channels int
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func startAudioSession(plugin *C.bridge_plugin_t, device string, hp hwparams.Params,
	midiCh <-chan [3]byte, ctlCh <-chan controlEvent, params *paramState, io *ioState, seq *seqState, rt *sharedConfig,
	level *levelMeter, diag *diagStats) (*audioSession, error) {

	cDev := C.CString(device)
	defer C.free(unsafe.Pointer(cDev))
	pcm := C.bridge_pcm_open(cDev, C.uint(hp.Channels), C.uint(hp.Rate), C.uint(hp.Period), C.uint(hp.Buffer))
	if pcm == nil {
		return nil, errBridge("bridge_pcm_open")
	}

	s := &audioSession{
		pcm:      pcm,
		period:   int(C.bridge_pcm_period(pcm)),
		channels: int(C.bridge_pcm_channels(pcm)),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	log.Printf("audio session opened: device=%s channels=%d rate=%d period=%d (requested period=%d buffer=%d)",
		device, s.channels, hp.Rate, s.period, hp.Period, hp.Buffer)

	go s.run(plugin, midiCh, ctlCh, params, io, seq, rt, hp.Rate, level, diag)
	return s, nil
}

func (s *audioSession) stop() {
	close(s.stopCh)
	<-s.doneCh
}

// run is the real-time render loop: drain MIDI/control events, render a
// block, write it out. Same discipline as push-hack-xenia's own run: every
// bridge_plugin_* call happens here, on this one goroutine.
func (s *audioSession) run(plugin *C.bridge_plugin_t, midiCh <-chan [3]byte, ctlCh <-chan controlEvent,
	params *paramState, io *ioState, seq *seqState, rt *sharedConfig, rate int, level *levelMeter, diag *diagStats) {
	defer close(s.doneCh)
	defer C.bridge_pcm_close(s.pcm)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if rc := C.bridge_set_realtime(50); rc != 0 {
		log.Printf("warning: SCHED_FIFO unavailable (rc=%d) — continuing SCHED_OTHER", rc)
	}

	stereo := make([]int16, s.period*2)
	wide := make([]int16, s.period*s.channels)
	budget := time.Duration(s.period) * time.Second / time.Duration(rate)

	const meterTau = 0.3
	meterDecay := math.Exp(-float64(s.period) / float64(rate) / meterTau)

	var blocks, xrunRetries, notesReceived, slowBlocks int64
	var maxPre, maxWrite time.Duration
	lastReport := time.Now()
	lastSelfTicks, _ := selfCPUTicks()
	lastSysTicks, _ := systemCPUTicks()
	lastPanelPoll := time.Time{}
	panelKey := C.CString("panel_state")
	defer C.free(unsafe.Pointer(panelKey))
	panelBuf := make([]byte, 4096)

	// track_voices poll — see trackmeter.go's doc. Same throttle/cadence
	// as panel_state just above (and for the same reason: this is the one
	// goroutine allowed to call into the plugin).
	voicesKey := C.CString("track_voices")
	defer C.free(unsafe.Pointer(voicesKey))
	trackVoicesBuf := make([]byte, 256)

	for {
		select {
		case <-s.stopCh:
			log.Printf("audio session stopped after %d blocks (%d xrun retries, %d notes, %d slow blocks)",
				blocks, xrunRetries, notesReceived, slowBlocks)
			return
		default:
		}

		blockStart := time.Now()

	drainMIDI:
		for {
			select {
			case msg := <-midiCh:
				notesReceived++
				cMsg := (*C.uint8_t)(unsafe.Pointer(&msg[0]))
				C.bridge_plugin_on_midi(plugin, cMsg, 3)
			default:
				break drainMIDI
			}
		}

	drainCtl:
		for {
			select {
			case ev := <-ctlCh:
				if !diag.getReady() {
					// Device is still in its ~22s boot window (see
					// mm_plugin.cpp's MmInstance::framesSinceCreate doc):
					// mm_set_param/on_midi silently no-op there. Every case
					// below also mutates this host's OWN shadow state
					// (params/seq) unconditionally, with no confirmation the
					// plugin actually applied it -- if that mutation went
					// through while the device call was dropped, the two
					// permanently diverge for the rest of the session (e.g.
					// a track switch "succeeds" on screen but the device's
					// real currentTrack never moves, so every later encoder
					// tweak silently lands on the wrong track). Drop the
					// whole event here instead, matching the plugin's own
					// "ignore everything during boot" behavior exactly, so
					// the two sides can never disagree.
					continue
				}
				switch ev.kind {
				case ctlPageJump:
					params.setPage(ev.idx)

				case ctlBankFlip:
					setBank(params, ev.idx)

				case ctlTrackStep:
					// D-Pad Up/Down: works on every page/bank, same
					// "always live" posture as Xenia's master-volume
					// encoder. Re-selecting the track on the plugin also
					// taps its front-panel Track button there (see
					// mm_plugin.cpp's selectTrack), and every slider must
					// then be resynced to that track's own values.
					if val, ok := params.NudgeTrack(ev.delta); ok {
						cSetParam(plugin, "track", val)
						params.syncFromPluginState(plugin)
					}

				case ctlToggleStep:
					track, step := ev.idx, ev.delta
					cSetParam(plugin, "toggle_step", fmt.Sprintf("%d,%d", track, step))
					seq.ToggleStep(track, step)
					// toggle_step on the plugin side also selects track
					// (mm_plugin.cpp's toggleStep -> selectTrack) if it
					// wasn't already current — mirror that here so the
					// "track" slot and every param slider follow which
					// row the user just tapped, same as a human pressing
					// a track-select button on real hardware before
					// programming its steps.
					if params.CurrentTrack() != track {
						params.SetParam("track", float64(track))
						params.syncFromPluginState(plugin)
					}
					params.MarkDirty()

				case ctlMuteToggle:
					cSetParam(plugin, "toggle_mute", strconv.Itoa(ev.idx))
					seq.ToggleMute(ev.idx)
					params.MarkDirty()

				case ctlLiveNote:
					// Shift+pad live audition on the SEQ page (main.go's
					// Fixed() doc). Unlike ctlToggleStep, on_midi has no
					// side-effecting track-select of its own — it always
					// targets whatever inst->currentTrack already is — so
					// this must explicitly switch track itself before
					// sending the note, and only on "on": switching again
					// on "off" could retarget the currently-viewed track
					// off the back of a stray/mismatched release (see
					// main.go's doc on why Note Off is accepted
					// unconditionally here, not gated on Shift).
					if ev.key == "on" {
						if params.CurrentTrack() != ev.idx {
							cSetParam(plugin, "track", strconv.Itoa(ev.idx))
							params.SetParam("track", float64(ev.idx))
							params.syncFromPluginState(plugin)
						}
						params.MarkDirty()
					}
					status := byte(0x80)
					velocity := byte(0)
					if ev.key == "on" {
						status = 0x90
						velocity = byte(ev.val)
					}
					msg := [3]byte{status, byte(ev.delta), velocity}
					C.bridge_plugin_on_midi(plugin, (*C.uint8_t)(unsafe.Pointer(&msg[0])), 3)

				case ctlBottomPress:
					switch params.Page() {
					case pageSeq:
						switch ev.idx {
						case 0: // PAGE: steps 1-8 <-> 9-16
							seq.TogglePage()
						case 1:
							doTransport(plugin, seq, "play")
						case 2:
							doTransport(plugin, seq, "stop")
						case 3:
							doTransport(plugin, seq, "record")
						}
						params.MarkDirty()
					case pageSettings:
						switch ev.idx {
						case 0:
							io.commitMIDI()
							params.MarkDirty()
						case 2:
							io.commitDevice()
							params.MarkDirty()
						case 4:
							io.commitChannel()
							params.MarkDirty()
						case 6:
							io.commitRecvChannel()
							params.MarkDirty()
						}
					}

				case ctlEncoder:
					switch params.Page() {
					case pageSeq:
						if ev.idx == 0 {
							// BASE CHANNEL — the only encoder bound on
							// SEQ (see params.go's bankPageNames doc for
							// why there was no room for it as its own
							// SETTINGS column). 0-15, plain +1/-1 per
							// tick (no acceleration throttling needed for
							// a 16-value range).
							ch := rt.getBaseChannel() + sign(ev.delta)
							if ch < 0 {
								ch = 0
							}
							if ch > 15 {
								ch = 15
							}
							rt.setBaseChannel(ch)
							cSetParam(plugin, "base_channel", strconv.Itoa(ch))
							if err := saveConfig(io.HackDir(), rt.snapshot()); err != nil {
								log.Printf("SEQ: saving %s: %v", configFileName, err)
							}
							params.MarkDirty()
						}
					case pageSettings:
						switch ev.idx {
						case 0:
							io.moveMIDICursor(ev.delta)
						case 2:
							io.moveDeviceCursor(ev.delta)
						case 4:
							io.moveChannelCursor(ev.delta)
						case 6:
							io.moveRecvChannelCursor(ev.delta)
						}
						params.MarkDirty()
					default:
						if key, val, ok := params.applyEncoder(ev.idx, ev.delta); ok {
							cSetParam(plugin, key, val)
						}
					}

				case ctlSetParam:
					if val, ok := params.SetParam(ev.key, ev.val); ok {
						if ev.key == "track" {
							cSetParam(plugin, "track", val)
							params.syncFromPluginState(plugin)
						} else {
							cSetParam(plugin, ev.key, val)
						}
					}

				case ctlTransport:
					// Web UI's transport buttons — same panel taps as the
					// SEQ page's bottom-screen buttons, but callable
					// regardless of which page Push's own screen is
					// currently showing (see this event's doc in main.go).
					doTransport(plugin, seq, ev.key)
					params.MarkDirty()

				case ctlStepPage:
					if ev.idx == 0 {
						seq.TogglePage()
					} else {
						seq.SetStepPage(ev.delta)
					}
					params.MarkDirty()

				case ctlBaseChannel:
					ch := int(ev.val)
					if ch < 0 {
						ch = 0
					}
					if ch > 15 {
						ch = 15
					}
					rt.setBaseChannel(ch)
					cSetParam(plugin, "base_channel", strconv.Itoa(ch))
					if err := saveConfig(io.HackDir(), rt.snapshot()); err != nil {
						log.Printf("web base_channel: saving %s: %v", configFileName, err)
					}
					params.MarkDirty()
				}
			default:
				break drainCtl
			}
		}

		// Throttled panel_state poll — see panelstate.go's doc for why
		// this is the only goroutine allowed to call into the plugin.
		// panelStatePollInterval (100ms) matches the display loop's own
		// redraw cadence, so pad LEDs/on-screen SEQ grid never lag behind
		// a fresher snapshot they could have had.
		if now := time.Now(); now.Sub(lastPanelPoll) >= panelStatePollInterval*time.Millisecond {
			lastPanelPoll = now
			if n := C.bridge_plugin_get_param(plugin, panelKey,
				(*C.char)(unsafe.Pointer(&panelBuf[0])), C.int(len(panelBuf))); n >= 0 {
				if snap, ok := decodePanelState(string(panelBuf[:n])); ok {
					globalPanelState.set(snap)
				}
			}
			if n := C.bridge_plugin_get_param(plugin, voicesKey,
				(*C.char)(unsafe.Pointer(&trackVoicesBuf[0])), C.int(len(trackVoicesBuf))); n >= 0 {
				var counts []int
				if err := json.Unmarshal(trackVoicesBuf[:n], &counts); err == nil {
					globalTrackActivity.set(counts)
				}
			}
		}

		C.bridge_plugin_render(plugin,
			(*C.int16_t)(unsafe.Pointer(&stereo[0])), C.int(s.period))

		blockPeak := 0.0
		for _, v := range stereo {
			if a := math.Abs(float64(v)) / 32768.0; a > blockPeak {
				blockPeak = a
			}
		}
		for i := range wide {
			wide[i] = 0
		}
		// push-hub's "not connected to output" bypass (docs/push-hub-
		// proposal.md): the plugin still renders every block above (so
		// envelopes/voice state don't jump when refocused), but wide stays
		// all-zero and the meter reads silent — true output bypass, not
		// just a UI/MIDI lockout. Defaults focused=true, so a hack run
		// without push-hub always reaches this copy exactly as before.
		if isFocused() {
			level.set(math.Max(level.get()*meterDecay, blockPeak))
			offset := rt.getChannelOffset()
			if offset < 0 || offset+1 >= s.channels {
				offset = 0
			}
			for f := 0; f < s.period; f++ {
				base := f*s.channels + offset
				wide[base] = stereo[f*2+0]
				if offset+1 < s.channels {
					wide[base+1] = stereo[f*2+1]
				}
			}
		} else {
			level.set(0)
		}

		preElapsed := time.Since(blockStart)
		if preElapsed > maxPre {
			maxPre = preElapsed
		}
		if preElapsed > budget {
			slowBlocks++
			log.Printf("SLOW BLOCK #%d: drain+render+expand took %v, budget %v",
				blocks, preElapsed, budget)
		}

		writeStart := time.Now()
		written := C.bridge_pcm_writei(s.pcm,
			(*C.int16_t)(unsafe.Pointer(&wide[0])), C.uint(s.period))
		writeElapsed := time.Since(writeStart)
		if writeElapsed > maxWrite {
			maxWrite = writeElapsed
		}

		if time.Since(lastReport) > 2*time.Second {
			var cpuPct float64
			if selfTicks, err := selfCPUTicks(); err == nil {
				if sysTicks, err := systemCPUTicks(); err == nil && sysTicks > lastSysTicks {
					cpuPct = float64(selfTicks-lastSelfTicks) / float64(sysTicks-lastSysTicks) * 100
					lastSelfTicks, lastSysTicks = selfTicks, sysTicks
				}
			}
			diag.setCPU(cpuPct)

			voicesBuf := make([]byte, 16)
			key := C.CString("active_voices")
			if n := C.bridge_plugin_get_param(plugin, key, (*C.char)(unsafe.Pointer(&voicesBuf[0])), C.int(len(voicesBuf))); n >= 0 {
				if v, err := strconv.Atoi(string(voicesBuf[:n])); err == nil {
					diag.setVoices(v)
				}
			}
			C.free(unsafe.Pointer(key))

			readyBuf := make([]byte, 4)
			readyKey := C.CString("ready")
			if n := C.bridge_plugin_get_param(plugin, readyKey, (*C.char)(unsafe.Pointer(&readyBuf[0])), C.int(len(readyBuf))); n > 0 {
				diag.setReady(readyBuf[0] == '1')
			}
			C.free(unsafe.Pointer(readyKey))

			log.Printf("progress: blocks=%d slow=%d maxPre=%v maxWrite=%v cpu=%.1f%% voices=%d",
				blocks, slowBlocks, maxPre, maxWrite, cpuPct, diag.getVoices())
			maxPre, maxWrite = 0, 0
			lastReport = time.Now()
		}

		if int(written) < 0 {
			xrunRetries++
			log.Printf("bridge_pcm_writei error (retry #%d): rc=%d", xrunRetries, written)
			continue
		}
		blocks++
	}
}

// doTransport taps the named panel transport button (a quick press+release,
// same as any other panel_button call — see mm_plugin.cpp's tapControl)
// and updates seq's own optimistic playing/recording shadow. Shared by
// the SEQ page's bottom-screen buttons and the web UI's transport
// controls (ctlTransport). action is "play", "stop", or "record"; unknown
// values are a no-op.
func doTransport(plugin *C.bridge_plugin_t, seq *seqState, action string) {
	switch action {
	case "play":
		cSetParam(plugin, "panel_button", "Play,down")
		cSetParam(plugin, "panel_button", "Play,up")
		_, rec := seq.Transport()
		seq.SetTransport(true, rec)
	case "stop":
		cSetParam(plugin, "panel_button", "Stop,down")
		cSetParam(plugin, "panel_button", "Stop,up")
		_, rec := seq.Transport()
		seq.SetTransport(false, rec)
	case "record":
		cSetParam(plugin, "panel_button", "Record,down")
		cSetParam(plugin, "panel_button", "Record,up")
		playing, rec := seq.Transport()
		seq.SetTransport(playing, !rec)
	}
}

func sign(v int) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}

func errBridge(what string) error {
	return &bridgeError{what: what, msg: C.GoString(C.bridge_last_error())}
}

type bridgeError struct {
	what string
	msg  string
}

func (e *bridgeError) Error() string { return e.what + ": " + e.msg }

type audioStatus struct {
	mu    sync.Mutex
	ready bool
	msg   string
}

func (s *audioStatus) set(ready bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready, s.msg = ready, msg
}

func (s *audioStatus) get() (ready bool, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready, s.msg
}

const msgWaitingForCard = "Loopback Audio driver not loaded.\nInstall/enable push-audio-loopback."

// msgWaitingForLive's step 3 calls out "(2nd input)" -- this hack writes
// to hw:Audio,1,1 (subdevice 1), a DIFFERENT PCM stream from push-hack-
// xenia's hw:Audio,1,0 (see config.go's defaultConfig), specifically so
// the two hacks don't fight over exclusive access to the same ALSA hw
// device when both run at once via push-hub. That means this needs its
// OWN separate Live track, routed to the loopback card's second input,
// not the same one push-hack-xenia's setup already uses.
const msgWaitingForLive = "1. Go to Push Audio Settings.\n" +
	"2. Select Devices.\n" +
	"3. Enable the 2nd input on \"Push Hack Virtual Audio PCM\"\n" +
	"   (subdevice 1 -- separate from puXenia's own input).\n" +
	"4. Select an audio track.\n" +
	"5. Set the track's input to that 2nd input.\n" +
	"6. Turn on Monitor In."

// watchHWParams is the top-level audio supervisor — identical shape to
// push-hack-xenia's own, plus threading seq through to startAudioSession.
func watchHWParams(cardID string, rt *sharedConfig, plugin *C.bridge_plugin_t,
	midiCh <-chan [3]byte, ctlCh <-chan controlEvent, params *paramState, io *ioState,
	status *audioStatus, seq *seqState, level *levelMeter, diag *diagStats, shutdown <-chan struct{}) {

	var sess *audioSession
	var lastParams hwparams.Params
	var lastDevice string
	haveSess := false
	lastState := ""

	logTransition := func(state string) {
		if lastState == state {
			return
		}
		lastState = state
		log.Print(state)
	}

	stopSession := func() {
		if haveSess {
			sess.stop()
			sess = nil
			haveSess = false
		}
	}
	defer stopSession()

	for {
		select {
		case <-shutdown:
			return
		default:
		}

		if !cardPresent(cardID) {
			logTransition("waiting for " + cardID + " (push-audio-loopback not loaded yet)")
			status.set(false, msgWaitingForCard)
			stopSession()
			if !sleepOrStop(waitPollInterval, shutdown) {
				return
			}
			continue
		}

		hp, ok, err := hwparams.Read(cardID)
		if err != nil {
			log.Printf("reading hw_params for %s: %v", cardID, err)
			status.set(false, msgWaitingForLive)
			stopSession()
			if !sleepOrStop(waitPollInterval, shutdown) {
				return
			}
			continue
		}
		if !ok {
			logTransition("waiting for Live to open " + cardID + "...")
			status.set(false, msgWaitingForLive)
			stopSession()
			if !sleepOrStop(waitPollInterval, shutdown) {
				return
			}
			continue
		}

		device := rt.getPCM()
		if !haveSess || hp != lastParams || device != lastDevice {
			stopSession()
			newSess, err := startAudioSession(plugin, device, hp, midiCh, ctlCh, params, io, seq, rt, level, diag)
			if err != nil {
				log.Printf("opening PCM %s: %v — will retry", device, err)
				// A real bridge_pcm_open failure (wrong/busy/nonexistent
				// device string -- e.g. a subdevice that doesn't exist,
				// or one another hack already has open) is NOT the same
				// situation as "Live hasn't opened its side yet"
				// (msgWaitingForLive, above) even though both used to
				// show the identical on-screen message -- indistinguishable
				// from the screen alone, reported as "audio not ready
				// which is a lie" when the real cause turned out to be an
				// ALSA device conflict between two of this project's own
				// hacks (see docs/audio-pipeline-debugging.md's section
				// 9). Show the actual device string and error instead so
				// this doesn't need a log-file round trip to diagnose
				// next time.
				status.set(false, fmt.Sprintf("Can't open audio device:\n%s\n%v", device, err))
				if !sleepOrStop(waitPollInterval, shutdown) {
					return
				}
				continue
			}
			sess = newSess
			haveSess = true
			lastParams = hp
			lastDevice = device
			logTransition("running")
			status.set(true, "")
		}

		if !sleepOrStop(steadyPollInterval, shutdown) {
			return
		}
	}
}

func sleepOrStop(d time.Duration, shutdown <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-shutdown:
		return false
	}
}
