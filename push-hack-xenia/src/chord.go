package main

// chord.go — Shift+Device (CC49+CC110, docs/push3-button-map.md) chord
// detection that toggles the on-screen param UI. Pattern mirrors
// hacks/keyboard-visualizer/src/chord.go's Shift+Note detector (itself
// modeled on push-manager's chordCCPressed/chordCCReleased) — same 500ms
// debounce, same held-set-of-two-CCs shape.

import (
	"net/http"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	ccShift  = uint8(push3.CCShift)
	ccDevice = uint8(push3.CCDeviceView)

	chordDebounce = 500 * time.Millisecond
)

var (
	chordMu       sync.Mutex
	chordHeld     = map[uint8]bool{}
	chordLastFire time.Time

	// hubPresent is set once at startup (main.go's runSupervised, before
	// any handler goroutine reads it) by probeHub. See docs/push-hub-
	// proposal.md's contract item 4 — plain bool, no locking needed since
	// it's write-once-then-read-only.
	hubPresent bool
)

// probeHub does a single short-timeout check for push-hub's /api/ping.
// Called once at startup, not polled continuously — see hubPresent's doc.
func probeHub(hubURL string) bool {
	client := http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(hubURL + "/api/ping")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// onChordCC is called for every channel-0 CC event. When Shift+Device are
// held together (debounced 500ms), toggles the on-screen param UI. Inert
// whenever push-hub is present — hub owns this chord exclusively then,
// and drives this hack's UI via POST /api/focus instead (webserver.go).
func onChordCC(cc, val byte, pmURL string, st *paramState, io *ioState, astatus *audioStatus, level *levelMeter, diag *diagStats) {
	if hubPresent {
		return
	}

	chordMu.Lock()
	if val > 0 {
		chordHeld[cc] = true
	} else {
		delete(chordHeld, cc)
	}
	fire := chordHeld[ccShift] && chordHeld[ccDevice]
	if fire {
		now := time.Now()
		if now.Sub(chordLastFire) < chordDebounce {
			fire = false
		} else {
			chordLastFire = now
		}
	}
	chordMu.Unlock()

	if fire {
		go toggleUI(pmURL, st, io, astatus, level, diag)
	}
}

// isShiftHeld reports whether Shift is currently held — used by the
// touch-strip pitch-bend handler to switch it to driving mod_wheel
// instead. Safe to call from the ALSA read-loop goroutine (same one that
// updates chordHeld).
func isShiftHeld() bool {
	chordMu.Lock()
	defer chordMu.Unlock()
	return chordHeld[ccShift]
}
