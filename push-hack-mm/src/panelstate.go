package main

// panelstate.go — a thread-safe cache of mm_plugin.cpp's get_param(
// "panel_state") snapshot (live trig-LED colors for the currently
// selected track, plus the raw LCD framebuffer for the web UI). Every
// bridge_plugin_* call must happen on audioSession.run's one goroutine
// (see audiosession.go's cSetParam doc and main.go's midiHandler doc for
// why — the C++ plugin instance is not thread-safe), so this holder is
// what lets the display loop and web server goroutines see a recent
// panel_state without calling into the plugin themselves: the render
// loop polls it periodically and publishes here; everyone else only
// reads.

import (
	"encoding/json"
	"sync"
)

type panelSnapshot struct {
	Track int    `json:"track"`
	Steps []int  `json:"steps"` // 16 entries, 0=off 1=green 2=red 3=yellow — FrontPanel::LedColor's order
	LCD   string `json:"lcd"`   // base64, 128x64 1bpp — passed through to the web UI as-is
}

type panelStateHolder struct {
	mu   sync.Mutex
	snap panelSnapshot
}

func (h *panelStateHolder) set(s panelSnapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snap = s
}

func (h *panelStateHolder) get() panelSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

// StepColor returns the selected track's step i's live LED color (0-3),
// or 0 (off) if i is out of range or no snapshot has landed yet.
func (s panelSnapshot) StepColor(i int) int {
	if i < 0 || i >= len(s.Steps) {
		return 0
	}
	return s.Steps[i]
}

// globalPanelState is written only from audioSession.run (via
// audiosession.go's own get_param("panel_state") call, throttled to
// panelStatePollInterval) and read from the display loop, leds.go's pad
// sync, and webserver.go's snapshot builder.
var globalPanelState = &panelStateHolder{}

func decodePanelState(jsonStr string) (panelSnapshot, bool) {
	var s panelSnapshot
	if err := json.Unmarshal([]byte(jsonStr), &s); err != nil {
		return panelSnapshot{}, false
	}
	return s, true
}

// panelStatePollInterval: fast enough that the SEQ page's pad LEDs (and a
// possible future playhead indicator) feel live, far below the render
// loop's own per-block rate. Matches display.go's own redraw cadence so
// neither is the bottleneck.
const panelStatePollInterval = 100
