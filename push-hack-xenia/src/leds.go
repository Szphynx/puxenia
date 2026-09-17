package main

// leds.go — LED control for the on-screen UI's chord/button surface (top
// screen 1-4 as page tabs, bottom screen 1-8 as page actions, D-Pad/Select
// dark since screen buttons replace them, Shift+Device lit white as the
// exit chord). pmclient has no LED methods, so this hits push-manager's
// raw HTTP API directly (POST /api/midi/led, same base URL as pmclient).
//
// push-manager's DELETE /api/midi/led/states only clears CCs its own
// internal exclusive/toggle logic tracked — a plain POST here never
// registers there, so releasing must send an explicit 0 for every CC this
// file ever lit, tracked in litCCs below, not rely on that endpoint.

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

var ledHTTP = &http.Client{Timeout: 2 * time.Second}

// setLEDCC posts one CC's LED value (0-127, Push's palette index — 0 is
// off). Best-effort: a failed LED write shouldn't take the UI down.
func setLEDCC(pmURL string, cc byte, value byte) {
	body := fmt.Sprintf(`{"type":"cc","channel":0,"cc":%d,"value":%d}`, cc, value)
	req, err := http.NewRequest(http.MethodPost, pmURL+"/api/midi/led", bytes.NewBufferString(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ledHTTP.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// paletteIdx resolves a push3 color name to its LED palette index — panics
// on a typo'd name, same posture as widgets' own paletteColor helper (only
// ever called with hand-checked names, so a typo should fail loud).
func paletteIdx(name string) byte {
	idx, ok := push3.ColorByName(name)
	if !ok {
		panic("leds: unknown push3 palette color " + name)
	}
	return idx
}

var (
	ledWhite = paletteIdx("white")
	ledDim   = paletteIdx("dgray")
	ledOff   = byte(0)
)

// litCCs is every control-surface CC except the 8 encoders (still
// functional, never blanked) — everything here goes dark the instant the
// UI opens, then only the current page's bound subset (see syncUILEDs)
// and Shift+Device get re-lit. Pads are never touched (see package doc's
// scope note) since they're notes, not CCs, and keep playing regardless.
// This is also the fixed release list on teardown, since push-manager's
// own state tracking doesn't see plain POST /api/midi/led writes (see
// package doc).
var litCCs = []byte{
	push3.CCScreenTop1, push3.CCScreenTop2, push3.CCScreenTop3, push3.CCScreenTop4,
	push3.CCScreenTop5, push3.CCScreenTop6, push3.CCScreenTop7, push3.CCScreenTop8,
	push3.CCScreenBot1, push3.CCScreenBot2, push3.CCScreenBot3, push3.CCScreenBot4,
	push3.CCScreenBot5, push3.CCScreenBot6, push3.CCScreenBot7, push3.CCScreenBot8,
	push3.CCVolume, push3.CCTempo, push3.CCTempoPress, push3.CCVolumePress,
	push3.CCJogPress, push3.CCJogClickLeft, push3.CCJogClickRight,
	push3.CCDPadUp, push3.CCDPadDown, push3.CCDPadLeft, push3.CCDPadRight, push3.CCDPadCenter,
	push3.CCSet, push3.CCSettings, push3.CCHelp, push3.CCUserMode,
	push3.CCDeviceView, push3.CCMixerView, push3.CCClipView, push3.CCSessionView,
	push3.CCShift, push3.CCSelect,
	push3.CCUndo, push3.CCSave, push3.CCAdd, push3.CCSwap,
	push3.CCLock, push3.CCStopClips, push3.CCMute, push3.CCSolo, push3.CCSelectMain,
	push3.CCTapTempo, push3.CCMetronome, push3.CCQuantize, push3.CCFixedLength,
	push3.CCAutomate, push3.CCNew, push3.CCCapture, push3.CCRecord, push3.CCPlay,
	push3.CCScene14, push3.CCScene14t, push3.CCScene18, push3.CCScene18t,
	push3.CCScene116, push3.CCScene116t, push3.CCScene132, push3.CCScene132t,
	push3.CCRepeat, push3.CCAccent, push3.CCScale, push3.CCLayout, push3.CCNote, push3.CCSession,
	push3.CCDoubleLoop, push3.CCDuplicate, push3.CCConvert, push3.CCDelete,
	push3.CCOctaveUp, push3.CCOctaveDown, push3.CCPageLeft, push3.CCPageRight,
}

// pageBottomLit says which bottom-row buttons (1-8, 1-indexed to match
// CCScreenBotN's own doc) are bound to an action on a given page — see
// params.go's page constants. Everything else on the bottom row goes dark.
var pageBottomLit = map[int][]int{
	pagePresets:  {1},
	pageSettings: {1, 3, 5},
}

// syncUILEDs blanks the whole control surface then lights only what's
// bound on the current page — top 1-4 as tabs (current page bright,
// others dim), Shift+Device white, and pageBottomLit's set for page. Called
// once when the UI turns on and again on every page/selection change.
func syncUILEDs(pmURL string, page int) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledOff)
	}
	for i := 0; i < len(pageNames); i++ {
		v := ledDim
		if i == page {
			v = ledWhite
		}
		setLEDCC(pmURL, push3.CCScreenTopN(i), v)
	}
	for _, n := range pageBottomLit[page] {
		setLEDCC(pmURL, push3.CCScreenBotN(n-1), ledWhite)
	}
	setLEDCC(pmURL, push3.CCShift, ledWhite)
	setLEDCC(pmURL, push3.CCDeviceView, ledWhite)
}

// releaseUILEDs turns every CC this file might have lit back off — called
// when the UI toggles off and on process shutdown, so a crash or normal
// exit never leaves Push's control surface stuck mid-override.
func releaseUILEDs(pmURL string) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledOff)
	}
}
