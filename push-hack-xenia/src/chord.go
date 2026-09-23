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
//
// Re-probes live (probeHub) rather than trusting the cached hubPresent —
// hubPresent is only ever set once, at this process's own boot (main.go),
// so a hack started BEFORE push-hub (or before push-hub was ever
// installed) would otherwise keep answering Shift+Device itself forever,
// racing push-hub's own reclaim on every press and never settling: "the
// screens fight endlessly," reported on real hardware, with no reliable
// key combo to escape it short of restarting this hack. A live probe on
// every chord fire is cheap (one ~200ms-timeout local HTTP call, on a
// user-paced action measured in seconds between presses, not a hot
// path) and makes Shift+Device self-healing regardless of start order —
// the moment push-hub exists, this hack notices on the very next press
// and stands down for good. Deliberately doesn't also update the
// package-level hubPresent here: nothing reads it again after main.go's
// own startup use, so there's no reason to add a second writer.
func onChordCC(cc, val byte, pmURL string, st *paramState, io *ioState, astatus *audioStatus, level *levelMeter, diag *diagStats) {
	if probeHub(defaultHubURL) {
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
