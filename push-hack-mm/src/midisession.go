package main

// midisession.go — one ALSA seq port, "Monomachine MIDI In", for
// everything: Push3's on-screen controls, the I/O picker's note source,
// any external gear/Live connecting in directly, AND (unlike
// push-hack-xenia) this hack's own pad-grid LED writes (leds.go) — the
// same client that reads pad presses also writes pad colors back out on
// it, since Push 3's pad RGB is just a Note On sent to the pad's own
// note number with velocity = palette index (push3/colors.go's doc), no
// separate LED protocol or push-manager endpoint needed. Content-based
// filtering (by src address) in main.go's midiHandler.Fixed sorts out
// which incoming event goes where — see that function's comments.
//
// Single persistent port instead of separate ones per role — see
// push-hack-xenia/src/midisession.go's doc for why (ALSA's CapSubsWrite/
// NO_EXPORT don't stop Live from listing a port; confirmed on hardware).

import (
	"log"
	"sync"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
)

// mmMIDIPortName — this hack's one MIDI port. iopage.go shows the same
// name in the I/O picker.
const mmMIDIPortName = "Monomachine MIDI In"

// midiClientHolder publishes the currently-open *alsaseq.Client so
// leds.go's pad-LED writer (running on the display-loop goroutine) can
// send Note On events back out on the same port main.go's handler reads
// pad presses from — set whenever watchMMPort (re)opens the port, cleared
// while it's down (a stale *Client would fail every write anyway, but a
// nil check reads clearer at the call site than a write erroring out).
type midiClientBox struct {
	mu  sync.Mutex
	seq *alsaseq.Client
}

func (b *midiClientBox) set(seq *alsaseq.Client) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq = seq
}

func (b *midiClientBox) get() *alsaseq.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

var midiClientHolder = &midiClientBox{}

// watchMMPort opens the port once (retries on failure) and keeps pulling
// from Push3's default port (pinned, for on-screen controls and pad
// input) plus whatever rt's note-input source is — adding a new
// Subscribe whenever that changes. Runs until shutdown fires.
func watchMMPort(rt *sharedConfig, handler alsaseq.Handler, shutdown <-chan struct{}) {
	pinned := alsaseq.Addr{Client: alsaseq.Push3ClientDefault, Port: alsaseq.Push3PortDefault}

	var seq *alsaseq.Client
	haveSeq := false
	subscribed := map[alsaseq.Addr]bool{}

	stop := func() {
		if haveSeq {
			midiClientHolder.set(nil)
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
			newSeq, err := openMMMIDIPort(handler)
			if err != nil {
				log.Printf("opening %s port: %v — will retry", mmMIDIPortName, err)
				if !sleepOrStop(waitPollInterval, shutdown) {
					return
				}
				continue
			}
			seq = newSeq
			haveSeq = true
			midiClientHolder.set(seq)
			log.Printf("%s port open — external MIDI gear or Live can connect to it directly", mmMIDIPortName)
			if err := seq.Subscribe(pinned); err != nil {
				log.Printf("subscribing %s to Push3 %v: %v", mmMIDIPortName, pinned, err)
			} else {
				subscribed[pinned] = true
				log.Printf("subscribed %s to %v for on-screen control surface and pad input", mmMIDIPortName, pinned)
			}
		}

		client, port := rt.getMIDI()
		target := alsaseq.Addr{Client: client, Port: port}
		if !subscribed[target] {
			if err := seq.Subscribe(target); err != nil {
				log.Printf("subscribing %s to %v for note input: %v — will retry", mmMIDIPortName, target, err)
			} else {
				subscribed[target] = true
				log.Printf("subscribed %s to %v for note input", mmMIDIPortName, target)
			}
		}

		if !sleepOrStop(steadyPollInterval, shutdown) {
			return
		}
	}
}

func openMMMIDIPort(handler alsaseq.Handler) (*alsaseq.Client, error) {
	seq, err := alsaseq.Open()
	if err != nil {
		return nil, err
	}
	if _, err := seq.CreatePort(mmMIDIPortName,
		alsaseq.CapWrite|alsaseq.CapSubsWrite, alsaseq.PortTypeMidi|alsaseq.PortTypeApp); err != nil {
		seq.Close()
		return nil, err
	}
	go func() {
		if err := seq.ReadLoop(handler); err != nil {
			log.Printf("%s read loop ended: %v", mmMIDIPortName, err)
		}
	}()
	return seq, nil
}
