package main

// iopage.go — the SETTINGS page: 4 independent columns (MIDI IN, AUDIO
// OUTPUT, AUDIO CHANNEL, MIDI CHANNEL), each 2 of the 8 encoder-slot
// columns wide, driven by encoders 1/3/5/7 and committed by bottom-screen
// buttons 1/3/5/7 (see audiosession.go's drainCtl and leds.go's
// pageBottomLit). Copied from push-hack-xenia/src/iopage.go essentially
// unchanged — none of this is Xenia-specific, it's the shared push-hack
// I/O picker pattern. The one MM-specific setting (BASE CHANNEL) lives on
// the SEQ page instead — see params.go's bankPageNames doc for why there
// was no room for a 5th column here.

import (
	"fmt"
	"image"
	"log"
	"sync"

	"github.com/federico-pepe/ableton-push-hack/core/alsapcm"
	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
)

const loopbackChannels = 32

type ioState struct {
	mu      sync.Mutex
	hackDir string
	rt      *sharedConfig

	midiCursor, deviceCursor, channelCursor, recvChCursor int
}

func newIOState(hackDir string, rt *sharedConfig) *ioState {
	return &ioState{hackDir: hackDir, rt: rt}
}

// HackDir returns the directory this hack's config file lives in — used
// by audiosession.go's SEQ-page BASE CHANNEL encoder to persist without
// duplicating hackDir in another package-level var.
func (io *ioState) HackDir() string { return io.hackDir }

type midiOptions struct {
	label string
	port  alsaseq.Port
}

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

func (io *ioState) saveLocked() {
	if err := saveConfig(io.hackDir, io.rt.snapshot()); err != nil {
		log.Printf("settings: saving %s: %v", configFileName, err)
	}
}

type ioOption struct {
	Label   string `json:"label"`
	Current bool   `json:"current"`
}

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
const settingsColW = 2 * cellW

func (io *ioState) render() *image.NRGBA {
	io.mu.Lock()
	defer io.mu.Unlock()

	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, widgets.Default.Black)
	renderTopTabs(img, widgets.Default, pageSettings)

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
	drawSettingsColumn(img, 0*settingsColW, "MIDI IN", midiLabels, io.midiCursor)

	deviceRows := io.buildDeviceRowsLocked()
	deviceLabels := make([]string, len(deviceRows))
	for i, r := range deviceRows {
		mark := "  "
		if r.device.HWDevice() == curDevice {
			mark = "> "
		}
		deviceLabels[i] = mark + r.label
	}
	drawSettingsColumn(img, 1*settingsColW, "AUDIO OUTPUT", deviceLabels, io.deviceCursor)

	channelRows := io.buildChannelRowsLocked()
	channelLabels := make([]string, len(channelRows))
	for i, r := range channelRows {
		mark := "  "
		if r.offset == curOffset {
			mark = "> "
		}
		channelLabels[i] = mark + r.label
	}
	drawSettingsColumn(img, 2*settingsColW, "AUDIO CHANNEL", channelLabels, io.channelCursor)

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
	drawSettingsColumn(img, 3*settingsColW, "MIDI CHANNEL", recvChLabels, io.recvChCursor)

	return img
}

func drawSettingsColumn(img *image.NRGBA, x int, title string, labels []string, cursor int) {
	t := widgets.Default
	text.Draw(img, x+4, 28, title, t.Gray)

	const top = 34
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
