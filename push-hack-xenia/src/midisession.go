package main

// midisession.go — one ALSA seq port, "Xenia MIDI In", for everything:
// Push3's on-screen controls, the I/O picker's note source, and any
// external gear/Live connecting in directly. Content-based filtering (by
// src address) in main.go's midiHandler.Fixed sorts out which event goes
// where — see that function's comments.
//
// Single persistent port instead of separate ones per role: this used to
// be 3 ports (2 "Push Xenia In" + 1 "Xenia MIDI In"), all 3 showing up
// as separate entries in Live's MIDI picker. ALSA's CapSubsWrite and
// NO_EXPORT bits (tried in that order) don't stop Live from listing a
// port — confirmed on hardware, twice. One real port is the only way to
// have one entry.

import (
	"log"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
)

// xeniaMIDIPortName — this hack's one MIDI port. iopage.go shows the same
// name in the I/O picker.
const xeniaMIDIPortName = "Xenia MIDI In"

// watchBraidsPort opens the port once (retries on failure) and keeps
// pulling from Push3's default port (pinned, for on-screen controls) plus
// whatever rt's note-input source is — adding a new Subscribe whenever
// that changes. Old subscriptions are never explicitly torn down (the
// underlying alsaseq.Client has no Unsubscribe); harmless, since
// midiHandler.Fixed decides what's "current" by content, not by which
// subscription delivered it. Runs until shutdown fires.
func watchBraidsPort(rt *sharedConfig, handler alsaseq.Handler, shutdown <-chan struct{}) {
	pinned := alsaseq.Addr{Client: alsaseq.Push3ClientDefault, Port: alsaseq.Push3PortDefault}

	var seq *alsaseq.Client
	haveSeq := false
	subscribed := map[alsaseq.Addr]bool{}

	stop := func() {
		if haveSeq {
			seq.Close()
			haveSeq = false
			subscribed = map[alsaseq.Addr]bool{}
		}
	}
	defer stop()

	for {
		select {
		case <-shutdown:
			return
		default:
		}

		if !haveSeq {
			newSeq, err := openBraidsMIDIPort(handler)
			if err != nil {
				log.Printf("opening %s port: %v — will retry", xeniaMIDIPortName, err)
				if !sleepOrStop(waitPollInterval, shutdown) {
					return
				}
				continue
			}
			seq = newSeq
			haveSeq = true
			log.Printf("%s port open — external MIDI gear or Live can connect to it directly", xeniaMIDIPortName)
			if err := seq.Subscribe(pinned); err != nil {
				log.Printf("subscribing %s to Push3 %v: %v", xeniaMIDIPortName, pinned, err)
			} else {
				subscribed[pinned] = true
				log.Printf("subscribed %s to %v for on-screen control surface", xeniaMIDIPortName, pinned)
			}
		}

		client, port := rt.getMIDI()
		target := alsaseq.Addr{Client: client, Port: port}
		if !subscribed[target] {
			if err := seq.Subscribe(target); err != nil {
				log.Printf("subscribing %s to %v for note input: %v — will retry", xeniaMIDIPortName, target, err)
			} else {
				subscribed[target] = true
				log.Printf("subscribed %s to %v for note input", xeniaMIDIPortName, target)
			}
		}

		if !sleepOrStop(steadyPollInterval, shutdown) {
			return
		}
	}
}

func openBraidsMIDIPort(handler alsaseq.Handler) (*alsaseq.Client, error) {
	seq, err := alsaseq.Open()
	if err != nil {
		return nil, err
	}
	if _, err := seq.CreatePort(xeniaMIDIPortName,
		alsaseq.CapWrite|alsaseq.CapSubsWrite, alsaseq.PortTypeMidi|alsaseq.PortTypeApp); err != nil {
		seq.Close()
		return nil, err
	}
	go func() {
		if err := seq.ReadLoop(handler); err != nil {
			log.Printf("%s read loop ended: %v", xeniaMIDIPortName, err)
		}
	}()
	return seq, nil
}
