package main

import "sync"

// sharedConfig is persistedConfig's live, in-memory counterpart: the
// values watchBraidsPort/watchHWParams actually act on, and the on-screen I/O
// page (iopage.go) writes to when the user picks a new port/device.
// Changing it takes effect on those supervisors' next poll tick — no
// process restart needed — and iopage.go persists it to xenia-config.json
// right after, so the choice survives a restart too.
type sharedConfig struct {
	mu             sync.Mutex
	midiClient     byte
	midiPort       byte
	pcmDevice      string
	channelOffset  int
	pushManagerURL string
	recvChannel    int
}

func newSharedConfig(cfg persistedConfig) *sharedConfig {
	return &sharedConfig{
		midiClient:     cfg.MidiClient,
		midiPort:       cfg.MidiPort,
		pcmDevice:      cfg.PCMDevice,
		channelOffset:  cfg.ChannelOffset,
		pushManagerURL: cfg.PushManagerURL,
		recvChannel:    cfg.RecvChannel,
	}
}

func (s *sharedConfig) getMIDI() (client, port byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.midiClient, s.midiPort
}

func (s *sharedConfig) setMIDI(client, port byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.midiClient, s.midiPort = client, port
}

func (s *sharedConfig) getPCM() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pcmDevice
}

func (s *sharedConfig) setPCM(device string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pcmDevice = device
}

func (s *sharedConfig) getChannelOffset() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.channelOffset
}

func (s *sharedConfig) setChannelOffset(offset int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channelOffset = offset
}

// getRecvChannel returns the MIDI channel (0-15) that non-Push3 Note
// On/Off must arrive on to trigger a voice, or -1 for omni (every
// channel). See persistedConfig.RecvChannel.
func (s *sharedConfig) getRecvChannel() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvChannel
}

func (s *sharedConfig) setRecvChannel(ch int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recvChannel = ch
}

// snapshot returns the current values as a persistedConfig, ready to save.
func (s *sharedConfig) snapshot() persistedConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return persistedConfig{
		MidiClient:     s.midiClient,
		MidiPort:       s.midiPort,
		PCMDevice:      s.pcmDevice,
		ChannelOffset:  s.channelOffset,
		PushManagerURL: s.pushManagerURL,
		RecvChannel:    s.recvChannel,
	}
}
