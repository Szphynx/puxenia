package main

// focus.go — the two exclusive actions push-hub can take on a registered
// hack: hand it (or take away) the screen/pad-input focus, and start or
// stop its underlying process. See docs/push-hub-proposal.md.

import (
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

var focusHTTP = &http.Client{Timeout: 1 * time.Second}

// setFocus POSTs the on/off state to one hack's /api/focus (added to both
// push-hack-xenia and push-hack-mm alongside this). Best-effort: a hack
// that's down or hasn't been patched with the contract just doesn't
// respond, which is exactly the "can't be focus-arbitrated" case the
// proposal doc already calls out -- not a reason to block the rest of the
// switch.
func setFocus(e hackEntry, on bool) {
	body := strings.NewReader(fmt.Sprintf(`{"on":%t}`, on))
	req, err := http.NewRequest(http.MethodPost, e.API+"/api/focus", body)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := focusHTTP.Do(req)
	if err != nil {
		log.Printf("focus %s (on=%v): %v", e.ID, on, err)
		return
	}
	resp.Body.Close()
}

// focusOnly defocuses every other currently-alive registered hack, then
// focuses target -- "check one synth or the other," never both at once,
// regardless of which one happened to be focused before. Runs on its own
// goroutine (main.go's Fixed() dispatches this with `go`) since it's a
// handful of blocking HTTP calls, not something the ALSA read loop should
// wait on.
func focusOnly(target hackEntry, all []hackStatus) {
	for _, st := range all {
		if st.ID == target.ID || !st.Alive {
			continue
		}
		setFocus(st.hackEntry, false)
	}
	setFocus(target, true)
}

// setServiceRunning starts or stops a registered hack's init.d service via
// the standard Debian `service <name> start|stop` wrapper.
//
// ASSUMPTION, not verified against real push-catalog install output: this
// repo's own docs (docs/push-hack-framework-notes.md) confirm push-catalog
// installs each hack as an init.d service launched via start-stop-daemon,
// but not the exact script name it assigns. hacks.json's "service" field
// is currently set to each hack's own binary name ("push-xenia"/
// "push-mm") as the best available guess -- if push-catalog names the
// installed script differently, fix hacks.json's "service" values to
// match rather than this code; the mechanism itself (service start/stop)
// should still be right.
func setServiceRunning(e hackEntry, on bool) error {
	action := "stop"
	if on {
		action = "start"
	}
	out, err := exec.Command("service", e.Service, action).CombinedOutput()
	if err != nil {
		return fmt.Errorf("service %s %s: %w (%s)", e.Service, action, err, strings.TrimSpace(string(out)))
	}
	return nil
}
