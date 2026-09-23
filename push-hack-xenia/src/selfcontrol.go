package main

// selfcontrol.go — RESTART/QUIT for this hack's own on-screen SETTINGS
// page (iopage.go), so it can be restarted/quit without going back to
// push-hub's menu first, per the user's own explicit request ("restart
// and quit... per device including the hub" — push-hub's own equivalent
// buttons live in push-hub/src/focus.go's restartSelf/quitSelf).
//
// Both rely entirely on the supervisor/child split main.go's
// runSupervisor already implements: this process (runSupervised) is
// always the CHILD of a runSupervisor instance that (a) unconditionally
// respawns the child whenever it exits on its own, and (b) only skips
// respawning when the SUPERVISOR's own signal handler is the one that
// caught SIGINT/SIGTERM and forwarded it down. So from inside the child:
//   - "restart" is just letting the child's own normal shutdown run and
//     exit -- the supervisor sees that as an unprompted exit and
//     respawns it, no different from a crash-and-retry.
//   - "quit" has to reach the SUPERVISOR's signal handler specifically
//     (not just this process's own), so the supervisor takes the
//     don't-respawn path -- sent to os.Getppid(), the supervisor's PID,
//     since runSupervisor starts this process directly via exec.Command.

import (
	"log"
	"os"
	"syscall"
)

// restartSelf signals this process's own SIGTERM handler (main.go's
// runSupervised) to run its normal graceful shutdown and exit -- the
// supervisor then respawns a fresh instance automatically, same as it
// would for a crash.
func restartSelf() {
	log.Printf("restart requested from SETTINGS page — shutting down, supervisor will respawn")
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		log.Printf("restart: signaling self: %v", err)
	}
}

// quitSelf signals the SUPERVISOR (this process's own parent) rather
// than itself, so the supervisor takes its own "signal received, forward
// and exit, don't respawn" path (main.go's runSupervisor) instead of
// treating this as an unprompted child exit.
func quitSelf() {
	log.Printf("quit requested from SETTINGS page — asking supervisor not to respawn")
	if err := syscall.Kill(os.Getppid(), syscall.SIGTERM); err != nil {
		log.Printf("quit: signaling supervisor (pid %d): %v", os.Getppid(), err)
	}
}
