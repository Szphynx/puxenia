package main

// iopage.go — the SETTINGS page: 3 independent columns (MIDI IN, AUDIO
// OUTPUT, AUDIO CHANNEL), each 2 of the 8 encoder-slot columns wide,
// driven by encoders 1/3/5 and committed by bottom-screen buttons 1/3/5
// (see audiosession.go's drainCtl and leds.go's pageBottomLit). Used to be
// one combined vertical list driven by D-Pad Up/Down + Select — replaced
// so each choice gets its own dedicated knob/button instead of sharing one
// cursor across all three.

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsapcm"
	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
)

// loopbackChannels is push-audio-loopback's fixed channel count (see that
// hack's README) — used only to offer a full range of channel pairs here,
// independent of whatever a live PCM session actually negotiated.
const loopbackChannels = 32

// midiMonitor is a raw-traffic debug indicator for the SETTINGS page's
// MIDI IN column. It records every event midiHandler.Fixed() sees from
// Push3, on whatever port, BEFORE any note-range/port/channel filtering --
// so it stays useful for answering "is anything arriving at all" even when
// the rest of the pipeline silently drops the event. Lock-free: written on
// the ALSA read-loop goroutine, read on the display goroutine.
type midiMonitor struct {
	count    atomic.Uint64
	lastNano atomic.Int64
	tagMu    sync.Mutex
	tag      string
}

func (m *midiMonitor) note(evType uint8, port byte, data []byte) {
	m.count.Add(1)
	m.lastNano.Store(time.Now().UnixNano())

	var kind, detail string
	switch evType {
	case alsaseq.EvNoteOn:
		kind, detail = "On", fmt.Sprintf("ch%d n%d v%d", data[0]&0x0F, data[1], data[2])
	case alsaseq.EvNoteOff:
		kind, detail = "Off", fmt.Sprintf("ch%d n%d", data[0]&0x0F, data[1])
	case alsaseq.EvController:
		cc := uint8(binary.LittleEndian.Uint32(data[4:]) & 0x7F)
		val := uint8(binary.LittleEndian.Uint32(data[8:]) & 0x7F)
		kind, detail = "CC", fmt.Sprintf("ch%d cc%d v%d", data[0]&0x0F, cc, val)
	case alsaseq.EvPitchBend:
		kind, detail = "PB", fmt.Sprintf("ch%d", data[0]&0x0F)
	default:
		kind = fmt.Sprintf("ev%d", evType)
	}

	tag := fmt.Sprintf("p%d %s %s", port, kind, detail)
	m.tagMu.Lock()
	m.tag = tag
	m.tagMu.Unlock()
}

// snapshot reports the last recorded event, whether it landed within the
// last 400ms (drives the on-screen activity dot), and the total count
// since boot (so "nothing lately" can still be told apart from "nothing
// ever").
func (m *midiMonitor) snapshot() (tag string, active bool, count uint64) {
	m.tagMu.Lock()
	tag = m.tag
	m.tagMu.Unlock()
	active = time.Since(time.Unix(0, m.lastNano.Load())) < 400*time.Millisecond
	count = m.count.Load()
	return
}

// ioState holds the SETTINGS page's 3 independent column cursors. Each
// column rebuilds its own option list fresh on every move/render (this
// page is opened rarely and ALSA's /proc reads are cheap) rather than
// caching, same posture as the page this replaces.
type ioState struct {
	mu      sync.Mutex
	hackDir string
	rt      *sharedConfig
	mon     *midiMonitor

	midiCursor, deviceCursor, channelCursor, recvChCursor int
}

func newIOState(hackDir string, rt *sharedConfig) *ioState {
	return &ioState{hackDir: hackDir, rt: rt, mon: &midiMonitor{}}
}

// midiOptions is one selectable row in the MIDI column.
type midiOptions struct {
	label string
	port  alsaseq.Port
}

// buildMIDIRowsLocked lists Push3's own 3 ports, the only real choices —
// see the historical footgun this filter avoids, still true here: an
// unfiltered readable port can look plausible but carry no pad/button
// events at all. Caller must hold io.mu.
func (io *ioState) buildMIDIRowsLocked() []midiOptions {
	ports, _ := alsaseq.EnumPorts(alsaseq.CapRead)
	var out []midiOptions
	for _, p := range ports {
		if p.Addr.Client != alsaseq.Push3ClientDefault {
			continue
		}
		out = append(out, midiOptions{label: p.PortName, port: p})
	}
	return out
}

type deviceOption struct {
	label  string
	device alsapcm.PlaybackDevice
}

func (io *ioState) buildDeviceRowsLocked() []deviceOption {
	devices, _ := alsapcm.EnumPlaybackDevices()
	out := make([]deviceOption, len(devices))
	for i, d := range devices {
		out[i] = deviceOption{label: fmt.Sprintf("%s (%s)", d.Name, d.HWDevice()), device: d}
	}
	return out
}

type channelOption struct {
	label  string
	offset int
}

func (io *ioState) buildChannelRowsLocked() []channelOption {
	var out []channelOption
	for ch := 0; ch+1 < loopbackChannels; ch += 2 {
		out = append(out, channelOption{label: fmt.Sprintf("Ch %d-%d", ch+1, ch+2), offset: ch})
	}
	return out
}

// recvChannelOption is one selectable row in the MIDI CHANNEL column —
// -1 (OMNI, the default) plus the 16 real MIDI channels. Restricts which
// channel's Note On/Off from non-Push3 sources (external gear, Live MIDI
// clips) triggers a voice — see main.go's Fixed() and
// sharedConfig.getRecvChannel.
type recvChannelOption struct {
	label   string
	channel int
}

func (io *ioState) buildRecvChannelRowsLocked() []recvChannelOption {
	out := []recvChannelOption{{label: "OMNI", channel: -1}}
	for ch := 0; ch < 16; ch++ {
		out = append(out, recvChannelOption{label: fmt.Sprintf("Ch %d", ch+1), channel: ch})
	}
	return out
}

// clampCursor keeps c in [0,n-1] (or 0 if n==0).
func clampCursor(c, n int) int {
	if n == 0 {
		return 0
	}
	if c < 0 {
		return 0
	}
	if c >= n {
		return n - 1
	}
	return c
}

// moveMIDICursor/moveDeviceCursor/moveChannelCursor step their column's
// cursor by delta's sign (magnitude ignored, matching the D-Pad-driven
// feel this page used to have — these lists are short enough that an
// encoder's accelerating delta doesn't need extra throttling).
func (io *ioState) moveMIDICursor(delta int) {
	io.mu.Lock()
	defer io.mu.Unlock()
	n := len(io.buildMIDIRowsLocked())
	io.midiCursor = clampCursor(io.midiCursor+sign(delta), n)
}

func (io *ioState) moveDeviceCursor(delta int) {
	io.mu.Lock()
	defer io.mu.Unlock()
	n := len(io.buildDeviceRowsLocked())
	io.deviceCursor = clampCursor(io.deviceCursor+sign(delta), n)
}

func (io *ioState) moveChannelCursor(delta int) {
	io.mu.Lock()
	defer io.mu.Unlock()
	n := len(io.buildChannelRowsLocked())
	io.channelCursor = clampCursor(io.channelCursor+sign(delta), n)
}

func (io *ioState) moveRecvChannelCursor(delta int) {
	io.mu.Lock()
	defer io.mu.Unlock()
	n := len(io.buildRecvChannelRowsLocked())
	io.recvChCursor = clampCursor(io.recvChCursor+sign(delta), n)
}

func sign(v int) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}

// commitMIDI/commitDevice/commitChannel apply the highlighted option in
// one column to sharedConfig and persist it — applying live (not just on
// next start) is what lets watchHWParams/watchBraidsPort pick it up on
// their next poll tick with no restart.
func (io *ioState) commitMIDI() {
	io.mu.Lock()
	defer io.mu.Unlock()
	rows := io.buildMIDIRowsLocked()
	if io.midiCursor < 0 || io.midiCursor >= len(rows) {
		return
	}
	row := rows[io.midiCursor]
	io.rt.setMIDI(row.port.Addr.Client, row.port.Addr.Port)
	io.saveLocked()
}

func (io *ioState) commitDevice() {
	io.mu.Lock()
	defer io.mu.Unlock()
	rows := io.buildDeviceRowsLocked()
	if io.deviceCursor < 0 || io.deviceCursor >= len(rows) {
		return
	}
	io.rt.setPCM(rows[io.deviceCursor].device.HWDevice())
	io.saveLocked()
}

func (io *ioState) commitChannel() {
	io.mu.Lock()
	defer io.mu.Unlock()
	rows := io.buildChannelRowsLocked()
	if io.channelCursor < 0 || io.channelCursor >= len(rows) {
		return
	}
	io.rt.setChannelOffset(rows[io.channelCursor].offset)
	io.saveLocked()
}

func (io *ioState) commitRecvChannel() {
	io.mu.Lock()
	defer io.mu.Unlock()
	rows := io.buildRecvChannelRowsLocked()
	if io.recvChCursor < 0 || io.recvChCursor >= len(rows) {
		return
	}
	io.rt.setRecvChannel(rows[io.recvChCursor].channel)
	io.saveLocked()
}

// saveLocked persists sharedConfig to xenia-config.json. Caller must hold io.mu.
func (io *ioState) saveLocked() {
	if err := saveConfig(io.hackDir, io.rt.snapshot()); err != nil {
		log.Printf("settings: saving %s: %v", configFileName, err)
	}
}

// ioOption is one selectable row, label plus whether it's the current
// choice — the web UI's JSON shape for the SETTINGS page's 3 columns (see
// webserver.go's handleIO). Mirrors what render()'s "> " marker shows
// on-screen.
type ioOption struct {
	Label   string `json:"label"`
	Current bool   `json:"current"`
}

// MIDIOptions/DeviceOptions/ChannelOptions list a column's selectable rows
// for the web UI — the same rows render() draws, without the display-only
// cursor highlight.
func (io *ioState) MIDIOptions() []ioOption {
	io.mu.Lock()
	defer io.mu.Unlock()
	curClient, curPort := io.rt.getMIDI()
	rows := io.buildMIDIRowsLocked()
	out := make([]ioOption, len(rows))
	for i, r := range rows {
		out[i] = ioOption{Label: r.label, Current: r.port.Addr.Client == curClient && r.port.Addr.Port == curPort}
	}
	return out
}

func (io *ioState) DeviceOptions() []ioOption {
	io.mu.Lock()
	defer io.mu.Unlock()
	curDevice := io.rt.getPCM()
	rows := io.buildDeviceRowsLocked()
	out := make([]ioOption, len(rows))
	for i, r := range rows {
		out[i] = ioOption{Label: r.label, Current: r.device.HWDevice() == curDevice}
	}
	return out
}

func (io *ioState) ChannelOptions() []ioOption {
	io.mu.Lock()
	defer io.mu.Unlock()
	curOffset := io.rt.getChannelOffset()
	rows := io.buildChannelRowsLocked()
	out := make([]ioOption, len(rows))
	for i, r := range rows {
		out[i] = ioOption{Label: r.label, Current: r.offset == curOffset}
	}
	return out
}

func (io *ioState) RecvChannelOptions() []ioOption {
	io.mu.Lock()
	defer io.mu.Unlock()
	curCh := io.rt.getRecvChannel()
	rows := io.buildRecvChannelRowsLocked()
	out := make([]ioOption, len(rows))
	for i, r := range rows {
		out[i] = ioOption{Label: r.label, Current: r.channel == curCh}
	}
	return out
}

// SetMIDIByIndex/SetDeviceByIndex/SetChannelByIndex commit column choice i
// directly (the web UI's equivalent of moving the on-screen cursor to i
// and pressing the column's commit button) — writes to sharedConfig and
// persists, same as commitMIDI/commitDevice/commitChannel.
func (io *ioState) SetMIDIByIndex(i int) error {
	io.mu.Lock()
	rows := io.buildMIDIRowsLocked()
	if i < 0 || i >= len(rows) {
		io.mu.Unlock()
		return fmt.Errorf("midi option index %d out of range (have %d)", i, len(rows))
	}
	io.midiCursor = i
	io.mu.Unlock()
	io.commitMIDI()
	return nil
}

func (io *ioState) SetDeviceByIndex(i int) error {
	io.mu.Lock()
	rows := io.buildDeviceRowsLocked()
	if i < 0 || i >= len(rows) {
		io.mu.Unlock()
		return fmt.Errorf("device option index %d out of range (have %d)", i, len(rows))
	}
	io.deviceCursor = i
	io.mu.Unlock()
	io.commitDevice()
	return nil
}

func (io *ioState) SetChannelByIndex(i int) error {
	io.mu.Lock()
	rows := io.buildChannelRowsLocked()
	if i < 0 || i >= len(rows) {
		io.mu.Unlock()
		return fmt.Errorf("channel option index %d out of range (have %d)", i, len(rows))
	}
	io.channelCursor = i
	io.mu.Unlock()
	io.commitChannel()
	return nil
}

func (io *ioState) SetRecvChannelByIndex(i int) error {
	io.mu.Lock()
	rows := io.buildRecvChannelRowsLocked()
	if i < 0 || i >= len(rows) {
		io.mu.Unlock()
		return fmt.Errorf("recv channel option index %d out of range (have %d)", i, len(rows))
	}
	io.recvChCursor = i
	io.mu.Unlock()
	io.commitRecvChannel()
	return nil
}

const settingsRowH = 13
const settingsColW = 2 * cellW // each of the 3 columns spans 2 of the 8 encoder slots

// render draws the 3 columns side by side, plus the MIDI IN and AUDIO
// OUTPUT debug indicators (raw-traffic tag/dot, live level bar) requested
// after "notes still don't play even on Live Port" made it clear the on-
// screen picker alone couldn't say whether bytes were arriving at all.
// screenW/screenH/cellW come from display.go.
func (io *ioState) render(level *levelMeter) *image.NRGBA {
	io.mu.Lock()
	defer io.mu.Unlock()

	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, widgets.Default.Black)
	renderTopTabs(img, widgets.Default, pageSettings)
	t := widgets.Default

	curClient, curPort := io.rt.getMIDI()
	curDevice := io.rt.getPCM()
	curOffset := io.rt.getChannelOffset()

	midiRows := io.buildMIDIRowsLocked()
	midiLabels := make([]string, len(midiRows))
	for i, r := range midiRows {
		mark := "  "
		if r.port.Addr.Client == curClient && r.port.Addr.Port == curPort {
			mark = "> "
		}
		midiLabels[i] = mark + r.label
	}
	drawColumnTitle(img, 0*settingsColW, "MIDI IN", t.Gray)
	tag, active, count := io.mon.snapshot()
	dotCol := t.Gray
	if active {
		dotCol = t.White
	}
	gfx.FillRect(img, 0*settingsColW+settingsColW-10, 22, 6, 6, dotCol)
	text.Draw(img, 0*settingsColW+4, 41, fmt.Sprintf("%d %s", count, tag), t.Gray)
	drawColumnRows(img, 0*settingsColW, 46, midiLabels, io.midiCursor)

	deviceRows := io.buildDeviceRowsLocked()
	deviceLabels := make([]string, len(deviceRows))
	for i, r := range deviceRows {
		mark := "  "
		if r.device.HWDevice() == curDevice {
			mark = "> "
		}
		deviceLabels[i] = mark + r.label
	}
	drawColumnTitle(img, 1*settingsColW, "AUDIO OUTPUT", t.Gray)
	gfx.FillRect(img, 1*settingsColW+4, 34, settingsColW-12, 4, t.Black)
	barW := int(dbFrac(level.get()) * float64(settingsColW-12))
	gfx.FillRect(img, 1*settingsColW+4, 34, barW, 4, t.White)
	drawColumnRows(img, 1*settingsColW, 46, deviceLabels, io.deviceCursor)

	channelRows := io.buildChannelRowsLocked()
	channelLabels := make([]string, len(channelRows))
	for i, r := range channelRows {
		mark := "  "
		if r.offset == curOffset {
			mark = "> "
		}
		channelLabels[i] = mark + r.label
	}
	drawColumnTitle(img, 2*settingsColW, "AUDIO CHANNEL", t.Gray)
	drawColumnRows(img, 2*settingsColW, 34, channelLabels, io.channelCursor)

	curRecvCh := io.rt.getRecvChannel()
	recvChRows := io.buildRecvChannelRowsLocked()
	recvChLabels := make([]string, len(recvChRows))
	for i, r := range recvChRows {
		mark := "  "
		if r.channel == curRecvCh {
			mark = "> "
		}
		recvChLabels[i] = mark + r.label
	}
	drawColumnTitle(img, 3*settingsColW, "MIDI CHANNEL", t.Gray)
	drawColumnRows(img, 3*settingsColW, 34, recvChLabels, io.recvChCursor)

	return img
}

// drawColumnTitle draws just a column's heading — split out from the row
// list so MIDI IN/AUDIO OUTPUT can insert their debug indicator between
// the two without duplicating the row-scrolling logic.
func drawColumnTitle(img *image.NRGBA, x int, title string, col color.NRGBA) {
	text.Draw(img, x+4, 28, title, col)
}

// drawColumnRows draws one column's scrollable row list starting at top —
// none of widgets' list helpers take an x-offset (they all draw at x=0
// spanning a caller-given width), so this is a small hand-rolled column
// renderer rather than 3 calls to widgets.RenderList.
func drawColumnRows(img *image.NRGBA, x int, top int, labels []string, cursor int) {
	t := widgets.Default
	visRows := (screenH - top) / settingsRowH
	scroll := cursor - visRows/2
	if scroll < 0 {
		scroll = 0
	}
	if maxScroll := len(labels) - visRows; maxScroll < 0 {
		scroll = 0
	} else if scroll > maxScroll {
		scroll = maxScroll
	}

	for i := 0; i < visRows; i++ {
		idx := scroll + i
		if idx >= len(labels) {
			break
		}
		y := top + i*settingsRowH
		col := t.White
		if idx == cursor {
			gfx.FillRect(img, x, y, settingsColW-4, settingsRowH, t.Select)
		}
		text.Draw(img, x+4, y+settingsRowH-3, labels[idx], col)
	}
}
