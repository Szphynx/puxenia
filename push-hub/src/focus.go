package main

// focus.go — the two exclusive actions push-hub can take on a registered
// hack: hand it (or take away) the screen/pad-input focus, and start or
// stop its underlying process. See docs/push-hub-proposal.md.

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// restartHackDelay is how long restartHack waits between stopping and
// starting -- long enough for the old process to actually release its
// ALSA device / listening port (both hack.json/hacks.json-registered
// hacks own real hardware handles, not just a socket) before the new one
// tries to claim them, short enough that RESTART still feels responsive
// from the menu.
const restartHackDelay = 1500 * time.Millisecond

// restartHack stops then starts a hack -- the RESTART bottom-screen
// button (main.go's CCScreenBot3), for the case setServiceRunning's own
// START/STOP toggle can't fix: a hack that's ALREADY running but started
// before push-hub did (or before push-hub was ever installed) never
// re-probes for the hub (each hack's own chord.go does that exactly once
// at boot -- see hubPresent's doc there) and keeps fighting it for
// Shift+Device/the screen until restarted. Reuses setServiceRunning for
// both halves so it gets the same service-then-direct-process fallback.
func restartHack(e hackEntry) error {
	if err := setServiceRunning(e, false); err != nil {
		log.Printf("restart %s: stop: %v", e.ID, err)
	}
	time.Sleep(restartHackDelay)
	return setServiceRunning(e, true)
}

// restartSelfDelay is how long the freshly-spawned push-hub instance
// waits before trying to bind its own port -- same reasoning as
// restartHackDelay, sized for a plain HTTP server + a few pmclient calls
// (shutdownHubUI) rather than an ALSA device, so it can be shorter.
const restartSelfDelay = "1"

// restartSelf spawns a fresh, detached push-hub (same binary, same
// working directory, same "-config hack.json" push-hub/deploy.sh always
// launches it with) that sleeps restartSelfDelay seconds before starting
// -- giving this process time to actually release port defaultHubPort --
// then quits this instance via quitSelf. There is no supervisor process
// for push-hub to relaunch it the way push-hack-xenia/push-hack-mm's own
// runSupervisor does (see their main.go) -- if this self-spawn fails for
// any reason, quitSelf still runs and push-hub stays down until manually
// restarted; the error is logged, not silently swallowed.
func restartSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: resolving own executable: %w", err)
	}
	dir := filepath.Dir(exe)
	bin := filepath.Base(exe)
	cmd := fmt.Sprintf("sleep %s && cd %s && nohup ./%s -config hack.json > push-hub.log 2>&1 &",
		restartSelfDelay, dir, bin)
	if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
		return fmt.Errorf("restart: spawning replacement: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	log.Printf("push-hub: replacement spawned (starts in %ss), quitting this instance", restartSelfDelay)
	quitSelf()
	return nil
}

// quitSelf asks this process's own SIGINT/SIGTERM handler (main.go) to
// run its normal graceful shutdown (shutdownHubUI etc.) -- sending the
// signal to ourselves rather than calling that shutdown path directly so
// there's exactly one shutdown code path regardless of whether it was
// triggered by the OS/an operator (a real `kill`/ssh session ending) or
// by this menu button.
func quitSelf() {
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		log.Printf("quit: signaling self: %v", err)
	}
}
