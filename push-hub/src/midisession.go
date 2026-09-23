package main

// midisession.go — one ALSA seq port, subscribed only to Push3's own
// default port. Unlike every DSP hack's own midisession.go, the hub never
// needs a note-input target (no synth of its own) and never exposes a
// port external gear would connect to -- it only needs to see Push3's
// control-surface CCs to run the menu.

import (
	"log"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
)

const hubMIDIPortName = "Push Hub Control"

// watchHubPort opens the port (retries on failure), subscribes to Push3's
// default port, and re-opens if the read loop ever ends -- same
// reopen-on-failure shape as every DSP hack's watch*Port, minus the
// note-input retargeting they need and this doesn't.
func watchHubPort(handler alsaseq.Handler, shutdown <-chan struct{}) {
	pinned := alsaseq.Addr{Client: alsaseq.Push3ClientDefault, Port: alsaseq.Push3PortDefault}

	for {
		select {
		case <-shutdown:
			return
		default:
		}

		seq, err := alsaseq.Open()
		if err != nil {
			log.Printf("opening ALSA seq client: %v — will retry", err)
			if !sleepOrStop(waitPollInterval, shutdown) {
				return
			}
			continue
		}
		if _, err := seq.CreatePort(hubMIDIPortName,
			alsaseq.CapWrite|alsaseq.CapSubsWrite, alsaseq.PortTypeMidi|alsaseq.PortTypeApp); err != nil {
			seq.Close()
			log.Printf("creating %s port: %v — will retry", hubMIDIPortName, err)
			if !sleepOrStop(waitPollInterval, shutdown) {
				return
			}
			continue
		}
		if err := seq.Subscribe(pinned); err != nil {
			log.Printf("subscribing %s to Push3 %v: %v", hubMIDIPortName, pinned, err)
		} else {
			log.Printf("%s subscribed to Push3's control surface", hubMIDIPortName)
		}

		if err := seq.ReadLoop(handler); err != nil {
			log.Printf("%s read loop ended: %v — reopening", hubMIDIPortName, err)
		}
		seq.Close()

		if !sleepOrStop(waitPollInterval, shutdown) {
			return
		}
	}
}
