package main

// cpu.go — push-hub's own CPU% (this process, plus overall system load)
// and "chokepoint" logging: anything on this process's own hot paths
// (polling a hack's /api/state, pushing a display frame) that takes
// unusually long, logged so a slow Push box shows up in the terminal
// instead of just "the menu feels laggy" with nothing to point at. Same
// /proc/self/stat + /proc/stat delta technique push-hack-xenia/push-hack-
// mm's own audiosession.go already uses for their own process CPU% --
// reproduced here rather than shared, since push-hub has no shared
// package with them to put it in.

import (
	"log"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// cpuSampleInterval matches pollRegistry's own cadence closely enough
// that a CPU spike and the poll cycle that caused it land in the same
// terminal-log neighborhood.
const cpuSampleInterval = 2 * time.Second

// chokepointThreshold flags any single poll/display-push call that takes
// this long or more -- well under pollOne's own 300ms client timeout and
// runHubDisplayLoop's 150ms tick interval, so a flagged call is a real
// slow chokepoint, not just this process's own normal cadence.
const chokepointThreshold = 100 * time.Millisecond

// hubSelfCPU/hubSystemCPU are lock-free live CPU% readings (this process,
// whole system), written by watchCPU below, read by renderMenu -- same
// write-heavy/read-light shape as registry.go's own hackStatus fields.
// atomic.Uint64 holding math.Float64bits, not atomic.Value, so a read
// never allocates or needs a type assertion.
var (
	hubSelfCPU   atomic.Uint64
	hubSystemCPU atomic.Uint64
)

func setHubCPU(selfPct, systemPct float64) {
	hubSelfCPU.Store(math.Float64bits(selfPct))
	hubSystemCPU.Store(math.Float64bits(systemPct))
}

// GetHubCPU returns this process's own CPU% and the whole system's, both
// 0 before the first sample (watchCPU's first tick, cpuSampleInterval
// after boot).
func GetHubCPU() (selfPct, systemPct float64) {
	return math.Float64frombits(hubSelfCPU.Load()), math.Float64frombits(hubSystemCPU.Load())
}

func selfCPUTicks() (uint64, error) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	s := string(data)
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return 0, os.ErrInvalid
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 13 {
		return 0, os.ErrInvalid
	}
	utime, _ := strconv.ParseUint(fields[11], 10, 64)
	stime, _ := strconv.ParseUint(fields[12], 10, 64)
	return utime + stime, nil
}

func systemCPUTicks() (uint64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			break
		}
		var total uint64
		for i := 1; i < 8; i++ {
			v, _ := strconv.ParseUint(f[i], 10, 64)
			total += v
		}
		return total, nil
	}
	return 0, os.ErrInvalid
}

// systemLoadPercent reads /proc/loadavg's 1-minute load average and
// scales it by CPU count into a rough 0-100(+) "system busy" percentage
// -- simpler and more directly meaningful for a small on-screen indicator
// than reconstructing idle-time share from /proc/stat's per-field deltas.
// Can read above 100 under real overload, same as `uptime`'s own load
// figure would imply; the caller/renderer clamps it for display.
func systemLoadPercent() float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	ncpu := runtime.NumCPU()
	if ncpu <= 0 {
		ncpu = 1
	}
	return load1 / float64(ncpu) * 100
}

// watchCPU samples this process's own CPU% (via the /proc/self/stat +
// /proc/stat tick-delta ratio, same technique as every DSP hack's own
// audiosession.go) and the system's overall load every cpuSampleInterval
// until shutdown fires. Own goroutine, started from main().
func watchCPU(shutdown <-chan struct{}) {
	ticker := time.NewTicker(cpuSampleInterval)
	defer ticker.Stop()

	lastSelf, _ := selfCPUTicks()
	lastSys, _ := systemCPUTicks()

	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
		}
		selfTicks, err := selfCPUTicks()
		if err != nil {
			continue
		}
		sysTicks, err := systemCPUTicks()
		if err != nil || sysTicks <= lastSys {
			continue
		}
		selfPct := float64(selfTicks-lastSelf) / float64(sysTicks-lastSys) * 100
		setHubCPU(selfPct, systemLoadPercent())
		lastSelf, lastSys = selfTicks, sysTicks
	}
}

// logChokepoint logs op if it took chokepointThreshold or longer --
// shared by pollOne and runHubDisplayLoop's own frame-push timing so both
// hot paths report slow calls the same way.
func logChokepoint(op string, elapsed time.Duration) {
	if elapsed >= chokepointThreshold {
		log.Printf("CHOKEPOINT: %s took %v (>= %v)", op, elapsed, chokepointThreshold)
	}
}
