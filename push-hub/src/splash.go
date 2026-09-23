package main

// splash.go — a big pulsing letter (hacks.json's "splash" field, "X" for
// puXenia / "MM" for puMMa) shown full-screen on the hub's own display
// while a hack the user just started/restarted is still loading, fading
// out over splashFadeDuration once it reports itself alive. Purely a
// hub-side transition effect: it never touches the target hack's own
// screen, only what push-hub itself pushes to push-manager while its own
// menu would otherwise be showing.

import (
	"image"
	"image/color"
	"math"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
)

// splashFadeDuration is how long the crossfade from the splash letter
// back to the normal hub menu takes, once the loading hack reports
// itself alive -- explicitly asked for as "about 2 seconds".
const splashFadeDuration = 2 * time.Second

// splashPulsePeriod is one full dim-to-bright-to-dim cycle while still
// loading.
const splashPulsePeriod = 1200 * time.Millisecond

// splashScale is DrawScaled's scale factor for the splash letter --
// empirically sized (see the baseline/centering comment on
// renderSplashFrame) to read clearly from across a room without any part
// of "MM" (the wider of the two letters) clipping the 960x160 screen.
const splashScale = 8

var (
	splashMu     sync.Mutex
	splashLetter string    // "" — no splash active
	splashID     string    // hackEntry.ID being waited on, matched against getStatuses()
	splashReady  time.Time // zero until the target first reports Alive; then the fade clock starts
)

// triggerSplash starts (or restarts) the loading splash for hack id,
// showing letter until that hack reports Alive in getStatuses(), then
// fading it out over splashFadeDuration. Called from main.go's START and
// RESTART actions -- not FOCUS, which (unlike START) usually hands off to
// an already-running, already-loaded process with nothing to wait on.
func triggerSplash(id, letter string) {
	if letter == "" {
		return // hacks.json has no "splash" configured for this entry -- nothing to show
	}
	splashMu.Lock()
	defer splashMu.Unlock()
	splashLetter = letter
	splashID = id
	splashReady = time.Time{}
}

// splashFrame is one render tick's resolved splash state: whether to draw
// it at all this frame, and if so, in pulsing or fading mode.
type splashFrame struct {
	active  bool
	letter  string
	pulsing bool    // true while still loading (pulse), false while fading out
	fadeT   float64 // 0 (just went alive, still full splash) .. 1 (fully faded, show menu) -- only meaningful when !pulsing
}

// currentSplashFrame resolves the splash state machine against the
// latest polled hack statuses (registry.go's pollRegistry) — called once
// per hub display tick (runHubDisplayLoop), same cadence as renderMenu.
func currentSplashFrame() splashFrame {
	splashMu.Lock()
	defer splashMu.Unlock()
	if splashLetter == "" {
		return splashFrame{}
	}

	alive := false
	for _, st := range getStatuses() {
		if st.ID == splashID && st.Alive {
			alive = true
			break
		}
	}

	if !alive {
		splashReady = time.Time{}
		return splashFrame{active: true, letter: splashLetter, pulsing: true}
	}

	if splashReady.IsZero() {
		splashReady = time.Now()
	}
	elapsed := time.Since(splashReady)
	if elapsed >= splashFadeDuration {
		splashLetter = "" // fade finished -- clear so future ticks skip straight to the menu
		return splashFrame{}
	}
	return splashFrame{
		active: true,
		letter: splashLetter,
		fadeT:  float64(elapsed) / float64(splashFadeDuration),
	}
}

// pulseBrightness returns the current phase of the loading pulse, 0..1,
// a smooth sine breathing rather than a hard blink -- clamped away from
// full 0 so the letter is always at least dimly visible (a "fully off"
// beat reads as a glitch, not a heartbeat).
func pulseBrightness() float64 {
	phase := float64(time.Now().UnixMilli()%splashPulsePeriod.Milliseconds()) / float64(splashPulsePeriod.Milliseconds())
	return 0.35 + 0.65*(0.5+0.5*math.Sin(2*math.Pi*phase))
}

// lerpColor blends a toward b by t (0=a, 1=b), channel-wise.
func lerpColor(a, b color.NRGBA, t float64) color.NRGBA {
	lerp := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.NRGBA{R: lerp(a.R, b.R), G: lerp(a.G, b.G), B: lerp(a.B, b.B), A: 255}
}

// renderSplashFrame draws letter centered on a plain backdrop, at the
// given brightness (0..1, lerped from black up to hubInk -- scaling
// hubInk's own RGB directly would also work since it's a light neutral
// color, but lerping from black reads as a cleaner "fade up" than
// scaling toward grey).
//
// Baseline/centering: empirically measured (a standalone render into an
// unclipped canvas, same method as the "PUSH HUB" title-clipping fix) --
// at splashScale=8, Tamzen7x13's glyphs for "X"/"MM" (no descenders) span
// 56px tall with their vertical midpoint ~36.5px above the baseline, so
// baseline=116 puts that midpoint at y=~79.5 on this 160px-tall screen,
// centered within a couple pixels. Horizontal centering uses each
// string's own real WidthScaled instead of a fixed offset, since "MM" is
// roughly twice as wide as "X".
func renderSplashFrame(letter string, brightness float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, hubBG)

	col := lerpColor(color.NRGBA{A: 255}, hubInk, brightness)
	w := text.WidthScaled(letter, splashScale)
	x := (screenW - w) / 2
	const baseline = 116
	text.DrawScaled(img, x, baseline, splashScale, letter, col)
	return img
}

// blendFrames crossfades a into b by t (0=a, 1=b) — plain per-channel
// linear interpolation between two already-fully-rendered frames of
// identical bounds, not real alpha compositing (image.NRGBA.Set doesn't
// blend against what's already there — see gfx/text's own doc on this,
// re-derived here rather than re-explained since this is the only place
// in this codebase that crossfades two whole frames instead of drawing
// translucent elements onto one). a and b must be the same size.
func blendFrames(a, b *image.NRGBA, t float64) *image.NRGBA {
	out := image.NewNRGBA(a.Bounds())
	for i := range out.Pix {
		av, bv := float64(a.Pix[i]), float64(b.Pix[i])
		out.Pix[i] = uint8(av + (bv-av)*t)
	}
	return out
}

// currentHubFrame is what runHubDisplayLoop actually pushes each tick --
// the plain menu, or the loading splash (pulsing, or crossfading back
// into the menu), depending on currentSplashFrame's state machine.
func currentHubFrame() *image.NRGBA {
	sf := currentSplashFrame()
	if !sf.active {
		return renderMenu()
	}
	if sf.pulsing {
		return renderSplashFrame(sf.letter, pulseBrightness())
	}
	return blendFrames(renderSplashFrame(sf.letter, 1.0), renderMenu(), sf.fadeT)
}
