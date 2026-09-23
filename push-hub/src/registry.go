package main

// registry.go — loads hacks.json (the static list of hacks push-hub can
// pick between, see docs/push-hub-proposal.md's "Registry" section) and
// polls each one's existing GET /api/state to build the menu's per-row
// status: alive/dead, focused, live VU level, and MIDI channel. A flat
// file is the right rung on the ladder for a handful of hacks — dynamic
// push-catalog directory scanning is explicitly deferred until that stops
// being true (see the proposal doc).

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// hackEntry is one hacks.json row. Service is the assumed init.d service
// name push-catalog installed this hack under (see focus.go's doc) --
// verify/adjust per entry if starting/stopping ever targets the wrong
// process. Dir/Exec/Process/Log are the direct-process-control fallback
// setServiceRunning uses when no such init.d service actually exists --
// this project's own deploy.sh/deploy-all.sh scripts never install one,
// they just nohup the binary directly (see each hack's own deploy.sh),
// so on a real checkout of this repo the "service" path always fails and
// this fallback is what actually starts/stops anything.
type hackEntry struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	API     string `json:"api"`
	Service string `json:"service"`
	Dir     string `json:"dir"`     // remote working directory -- deploy.sh's own REMOTE_DIR
	Exec    string `json:"exec"`    // command to run from Dir -- deploy.sh's own nohup command line
	Process string `json:"process"` // exact process name for pkill -x -- deploy.sh's own stop target
	Log     string `json:"log"`     // log file name, relative to Dir (defaults to "<id>.log" if empty)
}

// hackStatus is one polled snapshot, refreshed by pollRegistry below.
type hackStatus struct {
	hackEntry
	Alive   bool    `json:"alive"`
	Focused bool    `json:"focused"`
	Level   float64 `json:"level"`
	CPU     float64 `json:"cpu"`
	Channel string  `json:"channel"`
}

func loadRegistry(path string) ([]hackEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []hackEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

var (
	statusMu sync.Mutex
	statuses []hackStatus
)

func getStatuses() []hackStatus {
	statusMu.Lock()
	defer statusMu.Unlock()
	out := make([]hackStatus, len(statuses))
	copy(out, statuses)
	return out
}

// pollInterval matches the other hacks' own state-broadcast cadence
// closely enough that the hub's VU bars/alive dots don't look laggy next
// to a focused hack's own on-screen meter, without hammering a hack that
// might be unreachable.
const pollInterval = 250 * time.Millisecond

// pollRegistry refreshes every entry's status on a timer until shutdown
// fires. A single shared http.Client (short per-request timeout) means an
// unreachable/dead hack costs one timeout, not a hang.
func pollRegistry(entries []hackEntry, shutdown <-chan struct{}) {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	poll := func() {
		next := make([]hackStatus, len(entries))
		for i, e := range entries {
			next[i] = pollOne(client, e)
		}
		statusMu.Lock()
		statuses = next
		statusMu.Unlock()
	}
	poll() // don't wait a full tick for the first frame to have real data
	for {
		select {
		case <-shutdown:
			return
		case <-ticker.C:
			poll()
		}
	}
}

// pollOne hits one hack's GET /api/state. Every field below is read
// defensively (type-asserted, zero value on miss) since it's a different
// process's JSON shape that can legitimately omit or rename fields --
// an unreachable/erroring hack just comes back Alive:false, not a crash.
func pollOne(client *http.Client, e hackEntry) hackStatus {
	st := hackStatus{hackEntry: e}

	resp, err := client.Get(e.API + "/api/state")
	if err != nil {
		return st
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st
	}

	var m map[string]any
	if json.NewDecoder(resp.Body).Decode(&m) != nil {
		return st
	}
	st.Alive = true

	if f, ok := m["focused"].(bool); ok {
		st.Focused = f
	}
	if l, ok := m["level"].(float64); ok {
		st.Level = l
	}
	if diag, ok := m["diag"].(map[string]any); ok {
		if c, ok := diag["cpuPercent"].(float64); ok {
			st.CPU = c
		}
	}
	st.Channel = channelLabel(m)
	return st
}

// channelLabel best-effort extracts a MIDI-channel display string from a
// hack's /api/state -- push-hack-mm exposes it directly as "baseChannel"
// (0-indexed); push-hack-xenia only exposes it as the "current" row of
// its "io.recvChannel" option list (see push-hack-xenia/src/iopage.go's
// ioOption). Falls back to "--" rather than guessing a field name that
// isn't there.
func channelLabel(state map[string]any) string {
	if bc, ok := state["baseChannel"].(float64); ok {
		return "CH " + strconv.Itoa(int(bc)+1)
	}
	if io, ok := state["io"].(map[string]any); ok {
		if rows, ok := io["recvChannel"].([]any); ok {
			for _, row := range rows {
				opt, ok := row.(map[string]any)
				if !ok {
					continue
				}
				if cur, _ := opt["current"].(bool); cur {
					if label, ok := opt["label"].(string); ok {
						return label
					}
				}
			}
		}
	}
	return "--"
}
