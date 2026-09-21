package main

// display.go — draws the current page and pushes it to push-manager's
// display API, same architecture as push-hack-xenia/src/display.go (an
// HTTP client of push-manager only, never touching shared memory
// directly). Off by default: the UI only takes the screen (and enables
// push-manager's MIDI intercept for CC/button traffic) while toggled on
// via Shift+Device — see chord.go. Unlike Xenia, this file ALSO owns
// syncing the pad grid's own LEDs (via leds.go's syncPadLEDs) whenever
// the SEQ page is showing, and clearing them the instant it isn't.

import (
	"image"
	"image/color"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	screenW = push3.VisW
	screenH = push3.VisH

	cellW  = screenW / 8
	knobCX = 60
	knobCY = 78
	knobR  = 28

	botStripH = 16
	topStripH = 16
)

// Elektron Monomachine (SFX-60) panel palette: matte black chassis,
// off-white silkscreen, and the LCD's own pale green-on-dark look — not
// scanned from a reference photo, a best-effort approximation from
// general knowledge of the hardware's look (same caveat push-hack-xenia's
// display.go states for its own Microwave XT palette). Trig-key colors
// (green/red/amber) mirror the real machine's own 2-color (green/red)
// trig LEDs plus this host's own amber "yellow" mapping for
// FrontPanel::LedColor::Yellow.
var (
	mmChassis = color.NRGBA{R: 0x14, G: 0x14, B: 0x16, A: 255} // near-black chassis
	mmInk     = color.NRGBA{R: 0xD8, G: 0xD8, B: 0xD0, A: 255} // off-white print
	mmGreen   = color.NRGBA{R: 0x3C, G: 0xE0, B: 0x64, A: 255} // trig LED green
	mmRed     = color.NRGBA{R: 0xE8, G: 0x3C, B: 0x3C, A: 255} // trig LED red
	mmAmber   = color.NRGBA{R: 0xFF, G: 0xB0, B: 0x30, A: 255} // trig LED yellow / LCD glow
	mmTrack   = color.NRGBA{R: 0x2C, G: 0x2C, B: 0x30, A: 255} // knob track / empty step
)

var mmTheme = widgets.Theme{
	Black:    mmChassis,
	White:    mmInk,
	Gray:     mmInk,
	DarkGray: mmTrack,
	Select:   mmInk,
	Accent:   mmAmber,
	OnColor:  mmGreen,
	OffColor: mmTrack,
}

// knobColor groups by Monomachine panel section: FILTER in a cool blue-
// green (borrowed from mmGreen's hue family), AMP/SYNTH in ink (no
// accent), EFFECTS/LFO pages in amber (the "watch the value" pages).
func knobColor(key string) color.NRGBA {
	switch {
	case strings.HasPrefix(key, "filter_"):
		return mmGreen
	case strings.HasPrefix(key, "fx_") || strings.HasPrefix(key, "lfo"):
		return mmAmber
	default:
		return color.NRGBA{}
	}
}

var (
	uiMu     sync.Mutex
	uiOn     bool
	lastPage = -1

	// focused gates every Push3-sourced control/pad event in main.go's
	// Fixed() — see docs/push-hub-proposal.md. Defaults true so a hack
	// run without push-hub installed is unaffected; push-hub's
	// POST /api/focus (webserver.go's handleFocus) is the only thing
	// that ever sets it false.
	focused = true
)

func uiIsOn() bool {
	uiMu.Lock()
	defer uiMu.Unlock()
	return uiOn
}

func isFocused() bool {
	uiMu.Lock()
	defer uiMu.Unlock()
	return focused
}

func setFocused(v bool) {
	uiMu.Lock()
	focused = v
	uiMu.Unlock()
}

// renderTopTabs draws the page names across the top of the screen — spare
// bank-1 slots (params.go's bankPageNames, empty string) draw nothing,
// same skip leds.go's syncUILEDs already applies to their LED.
func renderTopTabs(img *image.NRGBA, t widgets.Theme, current int) {
	var top [8]widgets.SoftButton
	for i, name := range pageNames {
		if name == "" {
			continue
		}
		b := widgets.SoftButton{Label: strings.ToUpper(name)}
		if i == current {
			b.State = widgets.SoftOn
		}
		top[i] = b
	}
	widgets.DrawBotStrip(img, t, 0, screenW, cellW, topStripH, top, "")
}

// renderParamPage draws the current page: the "not ready" OSD first, else
// the knob grid, SEQ, or SETTINGS.
func renderParamPage(st *paramState, io *ioState, astatus *audioStatus, seq *seqState) *image.NRGBA {
	if ready, msg := astatus.get(); !ready {
		return renderWaitingScreen(msg)
	}
	switch st.Page() {
	case pageSeq:
		return renderSeqPage(st, seq)
	case pageSettings:
		return io.render()
	default:
		return renderKnobGrid(st)
	}
}

// renderKnobGrid draws one of the 7 per-track synth pages or the combined
// LEVEL page — same shape as push-hack-xenia's renderKnobGrid, minus the
// persistent master meter overlay (Xenia's channel_volume concept doesn't
// exist per-instance here; each track's own "level" knob IS the meter's
// reference point, drawn like any other knob, not a separate overlay).
func renderKnobGrid(st *paramState) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, mmChassis)
	renderTopTabs(img, mmTheme, st.Page())

	st.mu.Lock()
	var page []string
	if st.page >= 0 && st.page < len(paramPages) {
		page = paramPages[st.page]
	}
	type cell struct {
		key  string
		slot *paramSlot
	}
	cells := make([]cell, len(page))
	for i, key := range page {
		cells[i] = cell{key: key, slot: st.slots[key]}
	}
	track := 0
	if slot, ok := st.slots["track"]; ok {
		track = int(slot.value + 0.5)
	}
	st.mu.Unlock()

	trackLabel := "TRACK ?"
	if labels := trackLabels(); track >= 0 && track < len(labels) {
		trackLabel = "TRACK " + labels[track][1:]
	}
	text.Draw(img, screenW-70, topStripH+12, trackLabel, mmAmber)

	var bottom [8]widgets.SoftButton
	for i, c := range cells {
		if c.slot == nil {
			continue
		}
		bottom[i] = widgets.SoftButton{Label: strings.ToUpper(c.slot.meta.Name), Color: knobColor(c.key)}
		cx := i*cellW + knobCX

		if c.key == "mute" {
			renderMuteInset(img, cx, c.slot)
			continue
		}
		widgets.DrawKnobArc(img, mmTheme, cx, knobCY, knobR, widgets.Knob{
			Value: c.slot.value,
			Min:   c.slot.meta.Min,
			Max:   c.slot.meta.Max,
			Color: knobColor(c.key),
		})
	}
	widgets.DrawBotStrip(img, mmTheme, screenH-botStripH, screenW, cellW, botStripH, bottom, "")
	return img
}

func trackLabels() []string {
	names := make([]string, mmNumTracks)
	for i := range names {
		names[i] = "T" + string(rune('1'+i))
	}
	return names
}

func renderMuteInset(img *image.NRGBA, cx int, slot *paramSlot) {
	name := "OFF"
	col := mmGreen
	if slot.value >= 0.5 {
		name = "MUTE"
		col = mmRed
	}
	const boxW, boxH = cellW - 8, 36
	x, y := cx-boxW/2, knobCY-boxH/2
	gfx.FillRect(img, x, y, boxW, boxH, mmTrack)
	tw := text.Width(name)
	text.Draw(img, x+(boxW-tw)/2, y+boxH-11, name, col)
}

// renderSeqPage draws the SEQ page: a 6-row x 8-col step grid (this
// screen only ever shows the current 8-step page — see seq.StepPage())
// mirroring exactly what leds.go's syncPadLEDs is lighting on the pads
// themselves, plus BASE CHANNEL (encoder slot 0) and the 4 bottom-button
// labels (PAGE/PLAY/STOP/RECORD).
func renderSeqPage(st *paramState, seq *seqState) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, mmChassis)
	renderTopTabs(img, mmTheme, pageSeq)

	current := st.CurrentTrack()
	offset := seq.StepPage() * mmStepsPerPage
	playing, recording := seq.Transport()

	const gridTop = topStripH + 4
	const rowH = (screenH - botStripH - gridTop) / mmNumTracks
	const stepW = (screenW - 90) / mmStepsPerPage // leave a left gutter for track labels

	for row := 0; row < mmNumTracks; row++ {
		y := gridTop + row*rowH
		label := "T" + string(rune('1'+row))
		labelCol := mmInk
		if row == current {
			labelCol = mmAmber
		}
		text.Draw(img, 4, y+rowH-4, label, labelCol)
		if seq.Muted(row) {
			text.Draw(img, 30, y+rowH-4, "M", mmRed)
		}
		for col := 0; col < mmStepsPerPage; col++ {
			x := 60 + col*stepW
			led := stepLedColor(seq, st, row, col+offset)
			c := mmTrack
			if led != padStepOff && led != padStepOffOther {
				c = mmGreen
			}
			gfx.FillRect(img, x, y+2, stepW-3, rowH-6, c)
		}
	}

	head := "STEPS " + itoaSimple(offset+1) + "-" + itoaSimple(offset+mmStepsPerPage)
	if playing {
		head += "  > PLAYING"
	}
	if recording {
		head += "  [REC]"
	}
	text.Draw(img, 4, topStripH+12, head, mmInk)

	var bottom [8]widgets.SoftButton
	bottom[0] = widgets.SoftButton{Label: "PAGE"}
	bottom[1] = widgets.SoftButton{Label: "PLAY", Color: mmGreen}
	if playing {
		bottom[1].State = widgets.SoftOn
	}
	bottom[2] = widgets.SoftButton{Label: "STOP"}
	bottom[3] = widgets.SoftButton{Label: "REC", Color: mmRed}
	if recording {
		bottom[3].State = widgets.SoftOn
	}
	widgets.DrawBotStrip(img, mmTheme, screenH-botStripH, screenW, cellW, botStripH, bottom, "")
	return img
}

func itoaSimple(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func renderWaitingScreen(msg string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, widgets.Default.Black)
	text.DrawScaled(img, 8, 26, 2, "Monomachine not ready - setup needed", widgets.Default.White)
	y := 46
	for _, line := range strings.Split(msg, "\n") {
		text.Draw(img, 8, y, line, widgets.Default.Gray)
		y += 16
	}
	return img
}

// toggleUI flips the on-screen param UI, same shape as push-hack-xenia's
// own — plus clearing pad LEDs on the way out, since Xenia never lit pads
// at all.
// toggleUI flips the on-screen param UI — local Shift+Device's own path
// (chord.go). setUI does the actual work; both this and push-hub's
// POST /api/focus (webserver.go's handleFocus) call it directly rather
// than duplicating the takeover logic.
func toggleUI(pmURL string, st *paramState, io *ioState, astatus *audioStatus, seq *seqState, level *levelMeter) {
	uiMu.Lock()
	next := !uiOn
	uiMu.Unlock()
	setUI(pmURL, next, st, io, astatus, seq, level)
}

// setUI enters takeover mode (push-manager's display + MIDI intercept,
// current page's LEDs, an immediate frame) or releases all three back to
// the native Push UI / normal Live routing — an absolute set, not a
// toggle, so two independent callers (local Shift+Device and push-hub's
// HTTP-driven focus) never fight over one boolean's parity.
func setUI(pmURL string, on bool, st *paramState, io *ioState, astatus *audioStatus, seq *seqState, level *levelMeter) {
	uiMu.Lock()
	uiOn = on
	uiMu.Unlock()

	client := pmclient.New(pmURL)
	if on {
		if err := client.SetMode(2); err != nil {
			log.Printf("display: enable takeover: %v", err)
		}
		if err := client.SetMidiFilter(true); err != nil {
			log.Printf("display: enable midi filter: %v", err)
		}
		page := st.Page()
		syncUILEDs(pmURL, page)
		if page == pageSeq {
			syncPadLEDs(seq, st)
		}
		uiMu.Lock()
		lastPage = page
		uiMu.Unlock()
		if err := client.PushImage(renderParamPage(st, io, astatus, seq)); err != nil {
			log.Printf("display: push frame: %v", err)
		}
		log.Printf("puMMa: UI ON (Shift+Device) — MIDI intercept enabled")
	} else {
		if err := client.SetMode(0); err != nil {
			log.Printf("display: disable takeover: %v", err)
		}
		if err := client.SetMidiFilter(false); err != nil {
			log.Printf("display: disable midi filter: %v", err)
		}
		releaseUILEDs(pmURL)
		log.Printf("puMMa: UI OFF (Shift+Device) — MIDI intercept disabled")
	}
}

func shutdownUI(pmURL string) {
	client := pmclient.New(pmURL)
	_ = client.SetMode(0)
	_ = client.SetMidiFilter(false)
	releaseUILEDs(pmURL)
}

// runDisplayLoop redraws at ~10fps while the UI is on, and re-syncs LEDs
// (control-surface on page change; pad grid on EVERY tick while SEQ is
// active, since step/mute/track-highlight state can change from the web
// UI too, not just Push's own pads).
func runDisplayLoop(pmURL string, st *paramState, io *ioState, astatus *audioStatus, seq *seqState, level *levelMeter) {
	client := pmclient.New(pmURL)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		uiMu.Lock()
		on := uiOn
		uiMu.Unlock()
		if !on {
			continue
		}

		page := st.Page()
		uiMu.Lock()
		changed := page != lastPage
		if changed {
			lastPage = page
		}
		uiMu.Unlock()
		if changed {
			syncUILEDs(pmURL, page)
			if page != pageSeq {
				clearPadLEDs()
			}
		}
		if page == pageSeq {
			syncPadLEDs(seq, st)
		}

		if err := client.PushImage(renderParamPage(st, io, astatus, seq)); err != nil {
			log.Printf("display: push frame: %v", err)
		}
	}
}
