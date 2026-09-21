package main

// seq.go — this host's own shadow of the SEQ page's step/mute/transport
// state, driving the pad-grid LED sync (leds.go) and the web UI's step
// grid. mm_plugin.cpp/mdLib can only answer "what are the CURRENTLY
// SELECTED track's 16 step LEDs doing" (getMonomachineStepLedColor — real
// hardware only has one physical row of trig LEDs, it's inherently
// single-track), but this host's pad grid shows all 6 tracks' steps at
// once (rows 0-5), which has no hardware equivalent to read back. So: the
// selected track's row trusts the plugin's live panel_state poll (which
// also shows playhead flashing during playback — a real bonus this
// approach gets for free), and every OTHER row trusts this shadow, kept
// in sync because this host is the only thing that ever calls
// toggle_step/panel_track in the first place.

import "sync"

const mmNumSteps = 16
const mmStepsPerPage = 8

type seqState struct {
	mu        sync.Mutex
	stepPage  int // 0 or 1: pad columns show steps stepPage*8+1 .. stepPage*8+8
	steps     [mmNumTracks][mmNumSteps]bool
	mutes     [mmNumTracks]bool
	playing   bool
	recording bool
}

func newSeqState() *seqState { return &seqState{} }

// ToggleStep flips this host's own shadow bit and returns the new state —
// called right after mm_plugin.cpp's toggle_step call succeeds (main.go
// assumes it did; there's no ack path, same posture as every other
// fire-and-forget set_param call in this codebase).
func (s *seqState) ToggleStep(track, step int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if track < 0 || track >= mmNumTracks || step < 0 || step >= mmNumSteps {
		return false
	}
	s.steps[track][step] = !s.steps[track][step]
	return s.steps[track][step]
}

func (s *seqState) StepOn(track, step int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if track < 0 || track >= mmNumTracks || step < 0 || step >= mmNumSteps {
		return false
	}
	return s.steps[track][step]
}

func (s *seqState) SetStepPage(p int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == 0 || p == 1 {
		s.stepPage = p
	}
}

func (s *seqState) TogglePage() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stepPage = 1 - s.stepPage
	return s.stepPage
}

func (s *seqState) StepPage() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stepPage
}

func (s *seqState) ToggleMute(track int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if track < 0 || track >= mmNumTracks {
		return false
	}
	s.mutes[track] = !s.mutes[track]
	return s.mutes[track]
}

func (s *seqState) Muted(track int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if track < 0 || track >= mmNumTracks {
		return false
	}
	return s.mutes[track]
}

func (s *seqState) SetTransport(playing, recording bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.playing, s.recording = playing, recording
}

func (s *seqState) Transport() (playing, recording bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playing, s.recording
}

// Snapshot is the web UI's JSON shape for the SEQ page (webserver.go).
type seqSnapshot struct {
	StepPage  int      `json:"stepPage"`
	Steps     [][]bool `json:"steps"` // [track][step], all 16 steps regardless of stepPage
	Mutes     []bool   `json:"mutes"`
	Playing   bool     `json:"playing"`
	Recording bool     `json:"recording"`
}

func (s *seqState) Snapshot() seqSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := make([][]bool, mmNumTracks)
	mutes := make([]bool, mmNumTracks)
	for t := 0; t < mmNumTracks; t++ {
		row := make([]bool, mmNumSteps)
		copy(row, s.steps[t][:])
		steps[t] = row
		mutes[t] = s.mutes[t]
	}
	return seqSnapshot{StepPage: s.stepPage, Steps: steps, Mutes: mutes, Playing: s.playing, Recording: s.recording}
}
