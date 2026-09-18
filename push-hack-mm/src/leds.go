package main

// leds.go — LED control for the on-screen UI's control surface (top
// screen 1-8 as page tabs across 2 banks, bottom screen 1-8 as page
// actions), PLUS (new vs push-hack-xenia) the pad grid's own RGB LEDs
// while the SEQ page is active — see syncPadLEDs. The control-surface
// half still goes through push-manager's raw HTTP API
// (POST /api/midi/led) the same way push-hack-xenia's leds.go does; the
// pad half instead writes real Note On events straight out over this
// hack's own already-open ALSA seq port (midisession.go's
// midiClientHolder), because that's what Push 3's pad RGB actually is —
// a Note On to the pad's own note number with velocity = palette index
// (core/push3/colors.go's own doc comment: "Pad Note On: velocity =
// palette index → color") — not a push-manager display-API concern at
// all, so no dependency on push-manager for this half.

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

var ledHTTP = &http.Client{Timeout: 2 * time.Second}

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

	// Pad palette for SEQ mode — chosen to read clearly against Push's
	// black pad-well: FrontPanel::LedColor's 4 values (off/green/red/
	// yellow) map directly to matching named Push colors; the "off but
	// a valid step slot" dim backdrop and per-track/mute colors are this
	// host's own UI choice, not derived from real hardware LED colors
	// (Monomachine's own trig LEDs are red/green/off only — see
	// mdfrontpanel.h's LedColor doc; Push's fuller RGB palette is used
	// here purely for this host's own legibility, same spirit as
	// display.go's Microwave-XT-inspired palette in push-hack-xenia).
	padStepOff       = paletteIdx("dgray") // empty step slot, current track's row
	padStepOffOther  = paletteIdx("off")   // empty step slot, non-current track's row (dimmer still)
	padStepOn        = paletteIdx("green")
	padStepOnRed     = paletteIdx("red")
	padStepOnYellow  = paletteIdx("amber")
	padTrackSelected = paletteIdx("sky") // unused pad-well tint reserved for a future "track strip" — currently unused, see docs
	padMuteOn        = paletteIdx("red")
	padMuteOff       = paletteIdx("dgray")
)

var pageBottomLit = map[int][]int{
	pageSeq:      {1, 2, 3, 4},
	pageSettings: {1, 3, 5, 7},
}

// syncUILEDs blanks the whole control surface then lights only what's
// bound on the current page. Same shape as push-hack-xenia's own —
// unchanged except pageNames/pageBottomLit's own contents.
func syncUILEDs(pmURL string, page int) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledOff)
	}
	for i := 0; i < len(pageNames); i++ {
		if pageNames[i] == "" {
			continue // spare bank-1 slots (params.go's bankPageNames)
		}
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

func releaseUILEDs(pmURL string) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledOff)
	}
	clearPadLEDs()
}

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

// -- pad grid LEDs (SEQ page only) ------------------------------------

// setPadLED writes one pad's color via the live MIDI client
// (midisession.go's midiClientHolder) — a no-op (not an error) if the
// port isn't open yet, same "best-effort, shouldn't take the UI down"
// posture as setLEDCC.
func setPadLED(col, row int, colorIdx byte) {
	seq := midiClientHolder.get()
	if seq == nil {
		return
	}
	dst := alsaseq.Addr{Client: alsaseq.Push3ClientDefault, Port: alsaseq.Push3PortDefault}
	_ = seq.SendNote(dst, 0, push3.PadNote(col, row), colorIdx)
}

// clearPadLEDs turns every pad off — called when leaving SEQ page (so
// stale sequencer colors don't linger while pads go back to playing
// notes) and on UI teardown/shutdown.
func clearPadLEDs() {
	for row := 0; row < 8; row++ {
		for col := 0; col < 8; col++ {
			setPadLED(col, row, ledOff)
		}
	}
}

// stepLedColor picks a pad color for step i of track: the CURRENTLY
// SELECTED track's row trusts the plugin's live readback (globalPanelState,
// which also reflects playhead flashing during playback — real hardware's
// own trig LEDs do this too); every other row trusts this host's own
// shadow (seqState — see seq.go's doc for why there's no hardware
// readback for a non-selected track's steps).
func stepLedColor(seq *seqState, params *paramState, track, step int) byte {
	current := params.CurrentTrack()
	if track == current {
		switch globalPanelState.get().StepColor(step) {
		case 1:
			return padStepOn
		case 2:
			return padStepOnRed
		case 3:
			return padStepOnYellow
		default:
			return padStepOff
		}
	}
	if seq.StepOn(track, step) {
		return padStepOn
	}
	return padStepOffOther
}

// syncPadLEDs draws the SEQ page's full pad grid: rows 0-5 (bottom-up) =
// tracks 1-6's current 8-step page (seq.StepPage() selects steps 1-8 vs
// 9-16), row 6 dark/reserved, row 7 = per-track mute toggles (columns
// 0-5). Called every display tick while SEQ is the active page (see
// display.go's runDisplayLoop) — cheap enough for that cadence (48-54
// Note On writes at ~10fps).
func syncPadLEDs(seq *seqState, params *paramState) {
	offset := seq.StepPage() * mmStepsPerPage
	for row := 0; row < mmNumTracks; row++ {
		for col := 0; col < mmStepsPerPage; col++ {
			setPadLED(col, row, stepLedColor(seq, params, row, col+offset))
		}
	}
	for row := mmNumTracks; row < 7; row++ {
		for col := 0; col < 8; col++ {
			setPadLED(col, row, ledOff)
		}
	}
	for col := 0; col < 8; col++ {
		if col < mmNumTracks {
			if seq.Muted(col) {
				setPadLED(col, 7, padMuteOn)
			} else {
				setPadLED(col, 7, padMuteOff)
			}
		} else {
			setPadLED(col, 7, ledOff)
		}
	}
}
