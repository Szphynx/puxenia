package main

// trackmeter.go — per-track "is this voice sounding" indicator for
// display.go's SEQ page (row-by-row, all 6 tracks) and knob-grid pages
// (the currently selected track only). Not a true audio-level meter:
// mdLib's md::Device renders one summed stereo buffer with no separate
// per-track bus this host could read a real amplitude from — see
// audiosession.go's levelMeter for the one real level that IS available,
// the mixed master output (also what push-hub's own row VU bar and this
// hack's own SETTINGS-page meter show). What IS readable per-track is
// which MIDI channels currently have a held note (mm_plugin.cpp's
// activeNotes, keyed by channel = baseChannel+track — see mm_on_midi's
// remap), exposed via get_param("track_voices"): a JSON array of 6
// active-note counts, polled alongside panel_state in audiosession.go's
// run loop.
//
// A note's audible envelope tail outlasts its MIDI Note Off by a
// noticeable amount (especially on a percussive patch with a long amp
// release), and even a short trig can flicker for less than one display
// tick — so this holds each track "active" for trackMeterHold after its
// last nonzero reading, decaying the displayed brightness linearly across
// that window instead of a hard on/off. Same "reads as a meter, not a
// light switch" reasoning as push-hub's own splash pulseBrightness.

import (
	"sync"
	"time"
)

const trackMeterHold = 400 * time.Millisecond

type trackActivity struct {
	mu       sync.Mutex
	lastSeen [mmNumTracks]time.Time
}

var globalTrackActivity = &trackActivity{}

// set is called from audioSession.run's own poll tick (audiosession.go),
// same goroutine-ownership rule as panelstate.go's globalPanelState.set —
// counts is get_param("track_voices")'s decoded JSON array, one entry per
// track.
func (a *trackActivity) set(counts []int) {
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < mmNumTracks && i < len(counts); i++ {
		if counts[i] > 0 {
			a.lastSeen[i] = now
		}
	}
}

// level returns track i's current meter fraction (0-1), decaying linearly
// over trackMeterHold since its last active reading. 0 if the track has
// never been active or the hold window has fully elapsed.
func (a *trackActivity) level(track int) float64 {
	if track < 0 || track >= mmNumTracks {
		return 0
	}
	a.mu.Lock()
	last := a.lastSeen[track]
	a.mu.Unlock()
	if last.IsZero() {
		return 0
	}
	elapsed := time.Since(last)
	if elapsed >= trackMeterHold {
		return 0
	}
	return 1 - float64(elapsed)/float64(trackMeterHold)
}
