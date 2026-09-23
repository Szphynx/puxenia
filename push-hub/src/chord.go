package main

// chord.go — Shift+Device (CC49+CC110, docs/push3-button-map.md) chord
// detection, same 500ms-debounced shape as every other hack's own
// chord.go (e.g. push-hack-xenia/src/chord.go). The one real difference:
// push-hub owns this chord unconditionally, always. Every OTHER hack's
// own onChordCC stands down once it detects push-hub is present (see
// their chord.go's probeHub) -- this is the one binding that must always
// fire, since it's the only way back to the picker once a hack has taken
// the screen.

import (
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
)

// onChordCC reclaims the hub menu whenever Shift+Device fires. Also tells
// every registered hack to defocus first (defocusAll, focus.go) -- see
// its doc for why: setHubUI(true) alone is idempotent for the hub's own
// state, but leaves whichever hack was previously focused still believing
// it owns the screen, fighting the hub's own display loop for it.
func onChordCC(cc, val byte, pmURL string) {
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
		go func() {
			defocusAll(getStatuses())
			setHubUI(pmURL, true)
		}()
	}
}
