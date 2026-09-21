package main

// leds.go — lights the small set of controls the hub menu actually uses
// (D-Pad Up/Down for the cursor, bottom-screen buttons 1-2 for
// focus/start-stop, Shift+Device as the standing "back to hub" chord).
// Same push-manager raw-HTTP LED API every other hack's leds.go already
// uses -- see push-hack-xenia/src/leds.go's doc for why this bypasses
// pmclient (it has no LED methods) and why release must be an explicit
// list rather than push-manager's own state-tracked clear endpoint.

import (
	"bytes"
	"fmt"
	"net/http"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

var ledHTTP = &http.Client{Timeout: 2 * time.Second}

func setLEDCC(pmURL string, cc byte, value byte) {
	body := fmt.Sprintf(`{"type":"cc","channel":0,"cc":%d,"value":%d}`, cc, value)
	req, err := http.NewRequest(http.MethodPost, pmURL+"/api/midi/led", bytes.NewBufferString(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ledHTTP.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

func paletteIdx(name string) byte {
	idx, ok := push3.ColorByName(name)
	if !ok {
		panic("leds: unknown push3 palette color " + name)
	}
	return idx
}

var (
	ledWhite = paletteIdx("white")
	ledOff   = byte(0)
)

// litCCs is every CC this file ever lights -- also the fixed release list
// on menu-close/shutdown, since push-manager's own state tracking doesn't
// see plain POST /api/midi/led writes (see push-hack-xenia/src/leds.go).
var litCCs = []byte{
	push3.CCDPadUp, push3.CCDPadDown,
	push3.CCScreenBot1, push3.CCScreenBot2,
	push3.CCShift, push3.CCDeviceView,
}

func syncHubLEDs(pmURL string) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledWhite)
	}
}

func releaseHubLEDs(pmURL string) {
	for _, cc := range litCCs {
		setLEDCC(pmURL, cc, ledOff)
	}
}
