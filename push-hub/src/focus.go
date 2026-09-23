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

// defocusAll tells every currently-alive registered hack to release focus
// -- called when Shift+Device reclaims the hub menu (chord.go's
// onChordCC), before this process's own setHubUI(true). Without this, a
// hack that still thought it was focused (nothing ever told it
// otherwise -- reclaiming the hub only ever set the HUB's own uiOn, never
// touched the previously-focused hack's) kept believing it owned the
// screen even after the user explicitly asked to come back to the
// picker: both the hub's own runHubDisplayLoop and the hack's own display
// loop then independently kept re-asserting SetMode(2)+PushImage against
// the same push-manager, each undoing the other's frame every tick --
// visibly flickering back and forth, the reported "fights between xenia
// and the hub endlessly". Same "release before claim" ordering as the
// FOCUS-button fix in main.go's CCScreenBot1 case, just for the reverse
// direction.
func defocusAll(all []hackStatus) {
	for _, st := range all {
		if !st.Alive {
			continue
		}
		setFocus(st.hackEntry, false)
	}
}

// setServiceRunning starts or stops a registered hack. Tries the standard
// Debian `service <name> start|stop` wrapper first (in case this install
// really did go through push-catalog's init.d path), and falls back to
// direct process control (nohup relaunch / pkill -x) otherwise -- this
// repo's own deploy.sh/deploy-all.sh scripts (the actual, established way
// every hack in this project gets deployed) never register an init.d
// service at all, they just `nohup ./push-xenia &` (or push-mm) directly
// over ssh, so on a real checkout `service push-xenia start` always fails
// with "unrecognized service" and used to just silently do nothing --
// the reported "pressing Start does nothing".
func setServiceRunning(e hackEntry, on bool) error {
	action := "stop"
	if on {
		action = "start"
	}
	if e.Service != "" {
		if out, err := exec.Command("service", e.Service, action).CombinedOutput(); err == nil {
			return nil
		} else {
			log.Printf("service %s %s: %v (%s) -- falling back to direct process control",
				e.Service, action, err, strings.TrimSpace(string(out)))
		}
	}
	if on {
		return startDirect(e)
	}
	return stopDirect(e)
}

// stopDirect kills a hack's process directly -- -x (exact name match), not
// -f, same reasoning as every deploy.sh in this repo: -f matches full
// command lines, and pkill's own one-shot invocation here would otherwise
// risk matching its own wrapping process. pkill's exit code 1 means
// "nothing matched", which for a stop request is success, not an error.
func stopDirect(e hackEntry) error {
	if e.Process == "" {
		return fmt.Errorf("no \"process\" configured for %s in hacks.json -- can't stop it directly", e.ID)
	}
	out, err := exec.Command("pkill", "-x", e.Process).CombinedOutput()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("pkill -x %s: %w (%s)", e.Process, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// startDirect relaunches a hack the same way its own deploy.sh does:
// nohup'd, detached, logging to Dir/Log. sh -c backgrounds the command and
// exits immediately once it has, so CombinedOutput() returns quickly
// without waiting on the (now detached) hack process itself.
func startDirect(e hackEntry) error {
	if e.Dir == "" || e.Exec == "" {
		return fmt.Errorf("no \"dir\"/\"exec\" configured for %s in hacks.json -- can't start it directly", e.ID)
	}
	logFile := e.Log
	if logFile == "" {
		logFile = e.ID + ".log"
	}
	cmd := fmt.Sprintf("cd %s && nohup %s > %s 2>&1 &", e.Dir, e.Exec, logFile)
	out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
	if err != nil {
		return fmt.Errorf("direct start: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
