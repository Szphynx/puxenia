package main

// display.go — the hub's own on-screen menu: one row per registered hack
// (label, alive/dead dot, live VU bar, MIDI channel), a cursor moved by
// D-Pad Up/Down, bottom-screen button 1 to focus the highlighted row,
// button 2 to start/stop its service. Same push-manager HTTP client
// (pmclient) and takeover/MIDI-filter/PushImage pattern every other
// hack's display.go already uses -- see push-hack-xenia/src/display.go's
// doc for why that's the discipline (never touch the shared framebuffer
// directly).

import (
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

const (
	screenW   = push3.VisW
	screenH   = push3.VisH
	topStripH = 16
	botStripH = 16
	rowH      = 20
)

// hub palette: deliberately plain/neutral -- this isn't any one synth's
// panel, just a status readout, closer to a pedalboard's row of LEDs than
// an instrument face.
var (
	hubBG    = color.NRGBA{R: 0x10, G: 0x12, B: 0x18, A: 255}
	hubRow   = color.NRGBA{R: 0x28, G: 0x2C, B: 0x38, A: 255} // cursor highlight
	hubInk   = color.NRGBA{R: 0xE0, G: 0xE0, B: 0xE0, A: 255}
	hubDim   = color.NRGBA{R: 0x50, G: 0x54, B: 0x5C, A: 255} // dead/greyed-out
	hubGreen = color.NRGBA{R: 0x40, G: 0xD0, B: 0x70, A: 255} // alive dot + VU fill
)

var (
	uiMu   sync.Mutex
	uiOn   = true // the hub owns the screen from boot, until something is focused
	cursor int
)

func hubUIOn() bool {
	uiMu.Lock()
	defer uiMu.Unlock()
	return uiOn
}

func getCursor() int {
	uiMu.Lock()
	defer uiMu.Unlock()
	return cursor
}

// moveCursor wraps around both ends -- n changes at runtime as hacks come
// and go, so a fixed clamp could strand the cursor past the end of a
// shrunk list.
func moveCursor(delta int) {
	uiMu.Lock()
	defer uiMu.Unlock()
	n := len(getStatuses())
	if n == 0 {
		return
	}
	cursor = ((cursor+delta)%n + n) % n
}

// setHubUI is an absolute set, not a toggle -- same reasoning as every
// other hack's setUI (see push-hack-xenia/src/display.go's doc): the
// Shift+Device chord and a focused hack handing focus back both need to
// set this without racing on one boolean's parity.
func setHubUI(pmURL string, on bool) {
	uiMu.Lock()
	uiOn = on
	uiMu.Unlock()

	client := pmclient.New(pmURL)
	if on {
		if err := client.SetMode(2); err != nil {
			log.Printf("hub display: enable takeover: %v", err)
		}
		if err := client.SetMidiFilter(true); err != nil {
			log.Printf("hub display: enable midi filter: %v", err)
		}
		syncHubLEDs(pmURL)
		if err := client.PushImage(currentHubFrame()); err != nil {
			log.Printf("hub display: push frame: %v", err)
		}
		log.Printf("push-hub: menu ON (Shift+Device)")
	} else {
		if err := client.SetMode(0); err != nil {
			log.Printf("hub display: disable takeover: %v", err)
		}
		if err := client.SetMidiFilter(false); err != nil {
			log.Printf("hub display: disable midi filter: %v", err)
		}
		releaseHubLEDs(pmURL)
		log.Printf("push-hub: menu OFF -- focused hack owns the screen")
	}
}

func shutdownHubUI(pmURL string) {
	client := pmclient.New(pmURL)
	_ = client.SetMode(0)
	_ = client.SetMidiFilter(false)
	releaseHubLEDs(pmURL)
}

// runHubDisplayLoop redraws the menu at a plain UI refresh rate while it's
// showing -- no per-block audio meter to keep up with here, so this can
// be much slower than the DSP hacks' own ~10fps display loop.
func runHubDisplayLoop(pmURL string, shutdown <-chan struct{}) {
	client := pmclient.New(pmURL)
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
		}
		if !hubUIOn() {
			continue
		}
		// currentHubFrame, not renderMenu directly -- draws the loading
		// splash (splash.go) instead of/blended with the menu while one
		// is active, the plain menu otherwise.
		frameStart := time.Now()
		frame := currentHubFrame()
		if err := client.PushImage(frame); err != nil {
			log.Printf("hub display: push frame: %v", err)
		}
		logChokepoint("hub display frame", time.Since(frameStart))
	}
}

// renderMenu draws every registered hack as one row: cursor highlight,
// name (">" prefix if currently focused), an alive/dead dot, a VU bar
// (only ever non-empty while alive -- unfocused hacks are a true output
// bypass, see push-hack-xenia/src/audiosession.go, so an unfocused row's
// bar reads empty exactly like a bypassed pedal), and its MIDI channel.
func renderMenu() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, hubBG)
	// DrawScaled's y is the text BASELINE, and the glyph extends upward
	// from it by the face's ascent*scale (text.go's own doc) -- at scale
	// 2, Tamzen7x13's ascent puts the glyph's top ~2px above y=0 when
	// baseline=14, silently clipped off the real 0-origin screen buffer
	// (confirmed by rendering into an unclipped canvas: the glyph spans
	// y=[-2,11] at baseline=14). 18 clears it with a couple pixels of
	// margin, still well inside topStripH's reserved 16px band before the
	// first hack row starts at topStripH+14=30.
	text.DrawScaled(img, 8, 18, 2, "PUSH HUB", hubInk)

	// This process's own CPU% and the system's overall load (cpu.go's
	// watchCPU, sampled every cpuSampleInterval) -- top-right, clear of
	// the scaled title's own width even for the widest realistic value.
	selfPct, sysPct := GetHubCPU()
	cpuLabel := fmt.Sprintf("CPU %.0f%%  SYS %.0f%%", selfPct, sysPct)
	text.Draw(img, screenW-8-text.Width(cpuLabel), 14, cpuLabel, hubDim)

	list := getStatuses()
	cur := getCursor()
	top := topStripH + 14

	for i, st := range list {
		y := top + i*rowH
		if i == cur {
			gfx.FillRect(img, 0, y-12, screenW, rowH-2, hubRow)
		}

		ink := hubDim
		if st.Alive {
			ink = hubInk
		}
		label := st.Label
		if st.Focused {
			label = "> " + label
		}
		text.Draw(img, 6, y, label, ink)

		dot := hubDim
		if st.Alive {
			dot = hubGreen
		}
		gfx.FillRect(img, 150, y-9, 8, 8, dot)

		const vuX, vuW = 170, 60
		gfx.FillRect(img, vuX, y-9, vuW, 8, hubDim)
		if st.Alive && st.Level > 0 {
			// dbFrac, not the raw linear peak directly -- st.Level is the
			// same raw 0-1 peak amplitude push-hack-xenia/push-hack-mm's
			// own SETTINGS-page meters read, and they deliberately convert
			// it to a dB scale first (see dbFrac's own doc: normal program
			// material's raw peak sits well under 0.3, so a linear bar
			// reads as "stuck near empty" even with real, audible signal
			// present). Without this conversion here too, this row's bar
			// stayed a barely-visible sliver for perfectly normal audio —
			// reported as "I don't see audio coming out in the hub's VU
			// meter" even while that same hack's own meter (which DOES
			// apply dbFrac) showed clear activity.
			frac := dbFrac(st.Level)
			if w := int(float64(vuW) * frac); w > 0 {
				gfx.FillRect(img, vuX, y-9, w, 8, hubGreen)
			}
		}

		ch := st.Channel
		if ch == "" {
			ch = "--"
		}
		text.Draw(img, vuX+vuW+8, y, ch, ink)

		// Per-hack CPU%, already polled alongside Level/Channel
		// (registry.go's pollOne reads it from that hack's own
		// /api/state diag.cpuPercent) but never actually displayed
		// before now.
		if st.Alive {
			cpuLabel := fmt.Sprintf("%.0f%%", st.CPU)
			text.Draw(img, vuX+vuW+8+60, y, cpuLabel, ink)
		}
	}

	var bottom [8]widgets.SoftButton
	bottom[0] = widgets.SoftButton{Label: "FOCUS", State: widgets.SoftConfirm}
	startStop := "START"
	if cur < len(list) && list[cur].Alive {
		startStop = "STOP"
	}
	bottom[1] = widgets.SoftButton{Label: startStop}
	// RESTART (selected row) -- for a hack that's running but fighting the
	// hub for the screen because it started before the hub did (each
	// hack's own hub-detection probe only runs once, at boot -- see
	// hubPresent's doc in their own chord.go). See focus.go's restartHack.
	bottom[2] = widgets.SoftButton{Label: "RESTART"}
	// push-hub's OWN hide/restart/quit, not tied to the cursor row --
	// kept at the far right (5-7) so they read as "acting on the hub
	// itself," separate from the per-row group at 0-2.
	// ABLETON: hide the menu, hand the screen to Live, keep push-hub
	// running (Shift+Device brings the menu back) -- different from
	// QUIT, which actually stops the process. See main.go's
	// CCScreenBot6 case.
	bottom[5] = widgets.SoftButton{Label: "ABLETON"}
	bottom[6] = widgets.SoftButton{Label: "RE-HUB"}                       // see focus.go's restartSelf
	bottom[7] = widgets.SoftButton{Label: "QUIT", State: widgets.SoftOff} // see focus.go's quitSelf
	widgets.DrawBotStrip(img, widgets.Default, screenH-botStripH, screenW, screenW/8, botStripH, bottom, "")
	return img
}

// meterMinDB/dbFrac -- identical to push-hack-xenia's and push-hack-mm's
// own display.go: a linear 0-1 peak reads as "stuck near empty" on a
// linear bar for normal program material (nearly all its range spent
// near zero), so convert to a dB scale before drawing a row's VU bar
// above. -48dB floor maps to an empty bar, 0dB (full scale) to a full
// one.
const meterMinDB = -48.0

func dbFrac(peak float64) float64 {
	if peak <= 0 {
		return 0
	}
	db := 20 * math.Log10(peak)
	if db < meterMinDB {
		return 0
	}
	return (db - meterMinDB) / -meterMinDB
}
