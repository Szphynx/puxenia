// push-hub — a picker/launcher that arbitrates which one of several
// installed Push 3 synth hacks (puXenia, puMMa, ...) currently owns
// Push3's screen and pad/CC input. See docs/push-hub-proposal.md for the
// full design this implements.
//
// Unlike push-hack-xenia/push-hack-mm, this has no DSP plugin, no audio
// session, and no cgo -- it only ever talks to push-manager (screen/MIDI
// takeover, same as every other hack) and to the other hacks' own HTTP
// APIs (GET /api/state to poll status, POST /api/focus to arbitrate).
package main

import (
	"encoding/binary"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/hackcfg"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	defaultPushManagerURL = "http://localhost:7701"
	// defaultHubPort is the well-known port every other hack's chord.go
	// probes (GET /api/ping) to detect push-hub — see docs/push-hub-
	// proposal.md. Keep this in sync with push-xenia/push-mm's own
	// defaultHubURL constants if it ever changes.
	defaultHubPort = 7709

	waitPollInterval = time.Second
)

func sleepOrStop(d time.Duration, shutdown <-chan struct{}) bool {
	select {
	case <-time.After(d):
		return true
	case <-shutdown:
		return false
	}
}

// midiHandler implements alsaseq.Handler. The hub only ever cares about
// Push3's own control-surface CCs (Shift+Device always; D-Pad Up/Down and
// the two bottom-screen buttons only while its own menu is showing) --
// pad notes and pitch bend aren't its concern, it has no synth.
type midiHandler struct {
	pmURL string
}

func (h *midiHandler) Fixed(evType uint8, src alsaseq.Addr, data []byte) {
	if src.Client != alsaseq.Push3ClientDefault || src.Port != alsaseq.Push3PortDefault {
		return
	}
	if evType != alsaseq.EvController {
		return
	}
	// Control-surface CCs are always channel 0 -- see push-hack-xenia/
	// src/main.go's doc on why a held pad's own MPE stream needs this
	// same guard. The hub doesn't touch encoders (the actual MPE
	// collision range), but keeping the check makes this handler safe
	// regardless of what else Push3 is doing.
	if data[0]&0x0F != 0 {
		return
	}
	cc := uint8(binary.LittleEndian.Uint32(data[4:]) & 0x7F)
	val := uint8(binary.LittleEndian.Uint32(data[8:]) & 0x7F)

	if cc == ccShift || cc == ccDevice {
		onChordCC(cc, val, h.pmURL)
		return
	}

	if !hubUIOn() {
		return // a focused hack owns the screen/input right now, not the hub
	}
	if val != 127 {
		return // press-only, same as every other hack's button handling
	}
	switch cc {
	case push3.CCDPadUp:
		moveCursor(-1)
	case push3.CCDPadDown:
		moveCursor(1)
	case push3.CCScreenBot1:
		if list := getStatuses(); getCursor() < len(list) {
			target := list[getCursor()]
			go func() {
				// Release the hub's own takeover BEFORE focusing the
				// target, not after -- both setHubUI and the target
				// hack's own setUI (via /api/focus) call the SAME
				// push-manager's SetMode, and whichever call lands last
				// wins. The old order (focus, then release) meant the
				// hub's own SetMode(0) always fired last, clobbering the
				// target's SetMode(2) right back off -- the screen fell
				// through to push-manager's own idle state (Ableton's
				// screen) instead of showing the focused hack, exactly
				// the reported "pressing Focus just returns to Ableton's
				// screen".
				setHubUI(h.pmURL, false)
				focusOnly(target.hackEntry, list)
			}()
		}
	case push3.CCScreenBot2:
		if list := getStatuses(); getCursor() < len(list) {
			target := list[getCursor()]
			go func() {
				if err := setServiceRunning(target.hackEntry, !target.Alive); err != nil {
					log.Printf("start/stop %s: %v", target.ID, err)
				}
			}()
		}
	case push3.CCScreenBot3:
		// RESTART (selected row) -- for the case START/STOP alone can't
		// fix: a hack that's already running but started before push-hub
		// did never re-probes for it (each hack's own chord.go only
		// checks once, at boot) and keeps fighting the hub for Shift+
		// Device/the screen until restarted. See focus.go's restartHack.
		if list := getStatuses(); getCursor() < len(list) {
			target := list[getCursor()]
			go func() {
				if err := restartHack(target.hackEntry); err != nil {
					log.Printf("restart %s: %v", target.ID, err)
				}
			}()
		}
	case push3.CCScreenBot7:
		// RESTART (push-hub itself) -- see focus.go's restartSelf.
		go func() {
			if err := restartSelf(); err != nil {
				log.Printf("restart self: %v", err)
			}
		}()
	case push3.CCScreenBot8:
		// QUIT (push-hub itself) -- see focus.go's quitSelf.
		go quitSelf()
	}
}

func (h *midiHandler) VarLen(evType uint8, src alsaseq.Addr, payload []byte) {
	// Nothing the hub cares about — no synth, no need for SysEx/etc.
}

func main() {
	configPath := flag.String("config", "hack.json", "path to hack.json config file")
	flag.Parse()
	hackDir, err := filepath.Abs(filepath.Dir(*configPath))
	if err != nil {
		log.Fatalf("resolving hack dir: %v", err)
	}

	// Same cold-boot USB-A enumeration wait every other hack observes
	// before touching /dev/snd — see core/alsaseq/bootsettle.go.
	alsaseq.WaitForBootSettle()

	hcfg, err := hackcfg.Load(*configPath, defaultHubPort)
	if err != nil {
		log.Printf("loading %s: %v (defaulting port to %d)", *configPath, err, defaultHubPort)
		hcfg.Port = defaultHubPort
	}

	registryPath := filepath.Join(hackDir, "hacks.json")
	registry, err := loadRegistry(registryPath)
	if err != nil {
		log.Fatalf("loading %s: %v", registryPath, err)
	}
	log.Printf("push-hub: %d registered hack(s)", len(registry))

	pmURL := defaultPushManagerURL

	shutdown := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Printf("signal received (%v), stopping...", sig)
		close(shutdown)
	}()

	go pollRegistry(registry, shutdown)

	handler := &midiHandler{pmURL: pmURL}
	go watchHubPort(handler, shutdown)
	go runHubDisplayLoop(pmURL, shutdown)

	// Own the screen from boot — the whole point of a "dualboot" picker
	// is that it's the first thing shown, before anything is focused.
	setHubUI(pmURL, true)

	// Blocks until shutdown fires (server graceful-shutdown goroutine).
	runWebServer(hcfg.Port, shutdown)

	shutdownHubUI(pmURL)
	log.Printf("stopped")
}
