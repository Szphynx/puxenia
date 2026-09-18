package main

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
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

	"push-xenia/hwparams"
)

// cSetParam is bridge_plugin_set_param's CString/free boilerplate, shared
// by every drainCtl case that sends a param to the plugin.
func cSetParam(plugin *C.bridge_plugin_t, key, val string) {
	k, v := C.CString(key), C.CString(val)
	C.bridge_plugin_set_param(plugin, k, v)
	C.free(unsafe.Pointer(k))
	C.free(unsafe.Pointer(v))
}

// programDebounce coalesces rapid "program" (patch select) encoder ticks
// into a single device-facing commit after scrolling settles -- reported
// symptom: cycling through patches quickly made audio drop out and stay
// dropped. Real Microwave XT hardware can only ever receive one Program
// Change per however long a human takes to release a real front-panel
// encoder; its firmware's patch-load state machine was never built to
// handle a flood of them arriving faster than one load finishes. Our own
// on-screen encoder has no such natural limit -- a fast scroll can fire a
// dozen Program Changes within milliseconds, and if one lands mid-load of
// the previous one, that state machine (untested against this by its
// original authors) can wedge. The on-screen value still updates every
// tick via applyEncoder's own nudgeSlotLocked call (immediate, screen-only
// -- see params.go); only the actual device-facing MIDI commit is delayed
// here, and only for "program" (every other param already has its own
// per-param settle behavior via stepFor/sensitivityFor and isn't flooding
// the device at anywhere near this rate).
const programDebounceDelay = 200 * time.Millisecond

var (
	programDebounceMu    sync.Mutex
	programDebounceTimer *time.Timer
	// ctlChWrite is a writable handle onto the same channel main.go passes
	// everywhere else as receive-only (<-chan controlEvent) -- the timer
	// goroutine scheduleProgramCommit starts needs to send back into it,
	// which a <-chan-typed parameter can't do. Set once in main() right
	// after the channel is created, before anything starts using it.
	ctlChWrite chan<- controlEvent
)

func scheduleProgramCommit(val string) {
	programDebounceMu.Lock()
	defer programDebounceMu.Unlock()
	if programDebounceTimer != nil {
		programDebounceTimer.Stop()
	}
	fval, _ := strconv.ParseFloat(val, 64)
	programDebounceTimer = time.AfterFunc(programDebounceDelay, func() {
		select {
		case ctlChWrite <- controlEvent{kind: ctlSetParam, key: "program", val: fval}:
		default:
			log.Printf("control channel full, dropped debounced program commit")
		}
	})
}

// levelMeter is a lock-free live peak level (0-1), written every render
// block by audioSession.run, read at ~30fps by the display loop for the
// Volume fader — a plain atomic instead of a mutex since it's write-heavy
// on the real-time render thread and read-only everywhere else.
type levelMeter struct{ bits atomic.Uint64 }

func (m *levelMeter) set(v float64) { m.bits.Store(math.Float64bits(v)) }
func (m *levelMeter) get() float64  { return math.Float64frombits(m.bits.Load()) }

// diagStats is CPU%/active-voice-count diagnostics, same lock-free
// write-heavy/read-light shape as levelMeter -- written every ~2s by
// audioSession.run (piggybacked on the existing "progress" tick, not a
// separate timer), read by webserver.go's buildState for the browser UI's
// live SSE stream, so headroom for a future heavier emulation (Virus/TI)
// can be judged from real numbers instead of guessing from render-time
// budget alone.
type diagStats struct {
	cpuBits atomic.Uint64
	voices  atomic.Int32
}

func (d *diagStats) setCPU(pct float64) { d.cpuBits.Store(math.Float64bits(pct)) }
func (d *diagStats) getCPU() float64    { return math.Float64frombits(d.cpuBits.Load()) }
func (d *diagStats) setVoices(n int)    { d.voices.Store(int32(n)) }
func (d *diagStats) getVoices() int     { return int(d.voices.Load()) }

// selfCPUTicks reads this process's utime+stime (in clock ticks) from
// /proc/self/stat -- same field layout push-manager's own stats.go reads
// for its per-process CPU sampling (fields after the last ')' in the
// comm field, since comm itself can contain spaces/parens).
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

// systemCPUTicks reads the 7 CPU time fields from /proc/stat's "cpu " line
// (user nice sys idle iowait irq softirq) and returns their sum -- the
// same fields push-manager's readCPUSample reads for its own CPU%.
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
// it. The DSP plugin instance and MIDI subscription live independently in
// runSupervised and outlive any number of sessions — only the PCM side
// needs to restart when Live renegotiates its buffer or the user picks a
// different output device, since bridge.h already separates
// bridge_pcm_open/close from the plugin's own lifecycle.
type audioSession struct {
	pcm      *C.bridge_pcm_t
	period   int
	channels int
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// startAudioSession opens the PCM device and starts its dedicated render
// goroutine. rate is passed separately from hp because the caller decides
// which rate to request; in practice it is always hp.Rate.
func startAudioSession(plugin *C.bridge_plugin_t, device string, hp hwparams.Params,
	midiCh <-chan [3]byte, ctlCh <-chan controlEvent, params *paramState, io *ioState, rt *sharedConfig,
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

	// The plugin is created once at startup with a placeholder rate
	// (main.go's pluginInitRate) since the real one is only known once
	// Live has opened its side. Correct its resampler now that the true
	// negotiated rate is in hand -- otherwise audio plays back
	// pitched/timestretched by the ratio between the two, permanently.
	cSetParam(plugin, "_host_sample_rate", strconv.Itoa(int(hp.Rate)))

	go s.run(plugin, midiCh, ctlCh, params, io, rt, hp.Rate, level, diag)
	return s, nil
}

// stop signals the render goroutine and blocks until it has actually
// exited and closed the PCM handle — so the caller can safely open a new
// session against the same device right after this returns.
func (s *audioSession) stop() {
	close(s.stopCh)
	<-s.doneCh
}

// run is the real-time render loop: drain MIDI/control events, render a
// block, write it out. Each session gets its own goroutine pinned to its
// own OS thread — SCHED_FIFO (bridge_set_realtime) is a per-thread kernel
// attribute, not per-process, so it must be reapplied here on every new
// session, not just once at startup. Losing this on a session restart
// would silently reintroduce the "clean short taps, glitches on held
// notes" bug already fixed once (see main.go's history / CHANGELOG).
func (s *audioSession) run(plugin *C.bridge_plugin_t, midiCh <-chan [3]byte, ctlCh <-chan controlEvent,
	params *paramState, io *ioState, rt *sharedConfig, rate int, level *levelMeter, diag *diagStats) {
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

	// meterDecay is the level meter's per-block release factor — a 300ms
	// peak-hold time constant, converted to "how much to decay by" for a
	// block of this session's own period/rate rather than a fixed guess,
	// so the meter's feel doesn't change if Live renegotiates the buffer.
	const meterTau = 0.3
	meterDecay := math.Exp(-float64(s.period) / float64(rate) / meterTau)

	var blocks, xrunRetries, notesReceived, slowBlocks int64
	var maxPre, maxWrite time.Duration
	lastReport := time.Now()
	lastSelfTicks, _ := selfCPUTicks()
	lastSysTicks, _ := systemCPUTicks()

	for {
		select {
		case <-s.stopCh:
			log.Printf("audio session stopped after %d blocks (%d xrun retries, %d notes, %d slow blocks)",
				blocks, xrunRetries, notesReceived, slowBlocks)
			return
		default:
		}

		blockStart := time.Now()

		// Drain any MIDI that arrived since the last block. Every
		// bridge_plugin_* call happens here, on this one goroutine — see
		// midiHandler's doc comment in main.go for why that matters.
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

		// Drain any encoder turns / page jumps / bottom-button presses the
		// same way — applying them here keeps every bridge_plugin_* call
		// on this one goroutine, same reasoning as the MIDI drain above.
		// Dispatch is by current page: PRESETS and SETTINGS own encoders
		// 1/2 and 1/3/5 respectively (see params.go/iopage.go); every other
		// page uses the generic paramPages grid via applyEncoder.
	drainCtl:
		for {
			select {
			case ev := <-ctlCh:
				switch ev.kind {
				case ctlPageJump:
					params.setPage(ev.idx)

				case ctlBankFlip:
					setBank(params, ev.idx)

				case ctlMasterVolume:
					if val, ok := params.NudgeMasterVolume(ev.delta); ok {
						cSetParam(plugin, "channel_volume", val)
					}

				case ctlBottomPress:
					switch params.Page() {
					case pagePresets:
						if ev.idx == 0 { // bottom-1 = Load
							if idx, ok := params.loadStagedPreset(); ok {
								// "preset" is this host's own staged
								// browse-list key (see params.go's
								// fetchPresetMeta) -- Xenia has no
								// separate preset-apply call, so Load
								// actually just sends the real Program
								// Change via "program" instead.
								val := fmt.Sprintf("%d", idx)
								scheduleProgramCommit(val)
							}
						}
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
					case pagePresets:
						switch ev.idx {
						case 0:
							params.movePresetCursor(ev.delta)
						case 1:
							if val, ok := params.NudgeOctave(ev.delta); ok {
								cSetParam(plugin, "octave_transpose", val)
							}
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
							if key == "program" {
								scheduleProgramCommit(val)
							} else {
								cSetParam(plugin, key, val)
							}
						}
					}

				case ctlSetParam:
					// Absolute write from the web UI (see webserver.go) —
					// same goroutine, same cSetParam call as every other
					// case here, just not keyed off Push hardware. "preset"
					// is this host's own staged browse-list key (see
					// params.go's fetchPresetMeta) -- redirect the actual
					// device write to "program", same as the Load
					// button's path above.
					if val, ok := params.SetParam(ev.key, ev.val); ok {
						deviceKey := ev.key
						if deviceKey == "preset" {
							deviceKey = "program"
							// Keep the FILTER page's own "program" knob in
							// sync too, same as the Push-hardware Load
							// button path (audiosession.go's ctlBottomPress
							// case, via scheduleProgramCommit).
							params.SetParam("program", ev.val)
						}
						cSetParam(plugin, deviceKey, val)
					}
				}
			default:
				break drainCtl
			}
		}

		C.bridge_plugin_render(plugin,
			(*C.int16_t)(unsafe.Pointer(&stereo[0])), C.int(s.period))

		// Live level for the Volume fader (display.go) — instantaneous
		// peak this block, decayed against the last read for a standard
		// VU peak-hold look between the ~30fps display reads and this much
		// faster block rate. No allocation, one pass over already-decoded
		// samples — safe in this real-time loop.
		blockPeak := 0.0
		for _, v := range stereo {
			if a := math.Abs(float64(v)) / 32768.0; a > blockPeak {
				blockPeak = a
			}
		}
		level.set(math.Max(level.get()*meterDecay, blockPeak))

		for i := range wide {
			wide[i] = 0
		}
		// Which channel pair the stereo signal lands on is user-selectable
		// (I/O picker's "AUDIO CHANNEL" section) and read fresh every
		// block — applying it needs no PCM reopen, unlike a device or
		// hw_params change, since the channel count itself doesn't change.
		offset := rt.getChannelOffset()
		if offset < 0 || offset+1 >= s.channels {
			offset = 0 // defensive: picker only ever offers valid pairs for s.channels
		}
		for f := 0; f < s.period; f++ {
			base := f*s.channels + offset
			wide[base] = stereo[f*2+0]
			if offset+1 < s.channels {
				wide[base+1] = stereo[f*2+1]
			}
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
			// CPU%: this process's utime+stime delta over the system's
			// total ticks delta across the same window, same ratio
			// push-manager's own stats.go uses for its per-process CPU%
			// -- avoids needing to know USER_HZ (dividing proc-ticks by
			// system-total-ticks directly gives the fraction of all CPU
			// capacity this process used, whatever the tick rate is).
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

func errBridge(what string) error {
	return &bridgeError{what: what, msg: C.GoString(C.bridge_last_error())}
}

type bridgeError struct {
	what string
	msg  string
}

func (e *bridgeError) Error() string { return e.what + ": " + e.msg }

// audioStatus is watchHWParams' live readiness state. display.go reads it
// to pick real UI vs blocking "not ready" screen — no point drawing knobs
// when nothing's rendering audio yet.
type audioStatus struct {
	mu    sync.Mutex
	ready bool
	msg   string // reason, only meaningful when !ready
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

// 2 actionable "not ready" reasons — card missing, or Live hasn't opened
// it (README's "To actually hear it" has the exact Live-side steps).
const msgWaitingForCard = "Loopback Audio driver not loaded.\nInstall/enable push-audio-loopback."
const msgWaitingForLive = "1. Go to Push Audio Settings.\n" +
	"2. Select Devices.\n" +
	"3. Enable one input on \"Push Hack Virtual Audio PCM\".\n" +
	"4. Select an audio track.\n" +
	"5. Set the track's input to the input you enabled in Devices.\n" +
	"6. Turn on Monitor In."

// watchHWParams is the top-level audio supervisor: it waits for
// push-audio-loopback's card to exist, waits for Live to actually open its
// side, and (re)opens an audioSession whenever the negotiated params (or
// the target device) change — continuously, for the process's life, so
// Live restarting mid-session with different params is handled the same
// way as the very first negotiation. push-braids waits on
// push-audio-loopback's effect on kernel/ALSA state here, not on its
// process, because catalog's `requires` only orders installation, not
// boot-time service start order (see catalog/schema.md).
func watchHWParams(cardID string, rt *sharedConfig, plugin *C.bridge_plugin_t,
	midiCh <-chan [3]byte, ctlCh <-chan controlEvent, params *paramState, io *ioState,
	status *audioStatus, level *levelMeter, diag *diagStats, shutdown <-chan struct{}) {

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
			newSess, err := startAudioSession(plugin, device, hp, midiCh, ctlCh, params, io, rt, level, diag)
			if err != nil {
				log.Printf("opening PCM %s: %v — will retry", device, err)
				status.set(false, msgWaitingForLive)
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

// sleepOrStop waits for d or shutdown, whichever comes first. Returns
// false if shutdown fired, so callers can bail out immediately instead of
// finishing a stale wait.
func sleepOrStop(d time.Duration, shutdown <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-shutdown:
		return false
	}
}
