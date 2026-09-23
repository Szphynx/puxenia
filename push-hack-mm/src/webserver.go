package main

// webserver.go — optional browser control surface, alongside (not instead
// of) the on-Push-screen UI. Same goroutine-ownership rule as
// push-hack-xenia/src/webserver.go: reads go straight to paramState's/
// sharedConfig's/seqState's own mutex-guarded getters (safe from any
// goroutine); a write that reaches the plugin is queued as a
// controlEvent on ctlCh so the actual bridge_plugin_* call still happens
// on audioSession.run's one goroutine.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/httpx"
	"github.com/federico-pepe/ableton-push-hack/core/sse"
)

//go:embed ui/index.html
var webUI embed.FS

const stateBroadcastInterval = 100 * time.Millisecond

type webServer struct {
	version string
	pmURL   string
	params  *paramState
	io      *ioState
	astatus *audioStatus
	diag    *diagStats
	seq     *seqState
	level   *levelMeter
	ctl     chan<- controlEvent
	broker  *sse.Broker[[]byte]
}

func (ws *webServer) buildState() map[string]any {
	ready, msg := ws.astatus.get()
	snap := ws.params.Snapshot()
	seqSnap := ws.seq.Snapshot()
	panel := globalPanelState.get()
	return map[string]any{
		"version":  ws.version,
		"page":     snap.Page,
		"pageName": snap.PageName,
		"track":    snap.Track,
		"params":   snap.Params,
		"io": map[string]any{
			"midi":        ws.io.MIDIOptions(),
			"device":      ws.io.DeviceOptions(),
			"channel":     ws.io.ChannelOptions(),
			"recvChannel": ws.io.RecvChannelOptions(),
		},
		"seq":         seqSnap,
		"baseChannel": ws.io.rt.getBaseChannel(),
		"panel":       panel,
		"audio":       map[string]any{"ready": ready, "message": msg},
		"diag":        map[string]any{"cpuPercent": ws.diag.getCPU(), "activeVoices": ws.diag.getVoices(), "deviceReady": ws.diag.getReady()},
		"preset":      ws.diag.getPresetStatus(),
		// focused/level: push-hub's picker reads these (GET /api/state) to
		// show this hack as "alive" (VU meter + channel) or greyed out/
		// disconnected — see docs/push-hub-proposal.md and display.go's
		// focused var.
		"focused": isFocused(),
		"level":   ws.level.get(),
	}
}

func (ws *webServer) handleState(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, ws.buildState())
}

func (ws *webServer) handleParams(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, map[string]any{
		"params": ws.params.Metas(),
		"pages":  webParamPages(),
	})
}

func (ws *webServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	ch := ws.broker.Register()
	defer ws.broker.Unregister(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	if data, err := json.Marshal(ws.buildState()); err == nil {
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	sse.Serve(w, r, ch, func(b []byte) ([]byte, error) { return b, nil })
}

// handleSetParam queues an absolute param write for the CURRENTLY
// SELECTED track (or a track switch, if key=="track") — the actual
// bridge_plugin_set_param call happens on audioSession.run's goroutine
// (audiosession.go's drainCtl, ctlSetParam case).
func (ws *webServer) handleSetParam(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !ws.params.HasParam(key) {
		http.Error(w, "unknown param "+key, http.StatusNotFound)
		return
	}
	var body struct {
		Value float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlSetParam, key: key, val: body.Value}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleFocus is push-hub's focus/defocus call (docs/push-hub-proposal.md):
// an absolute on/off, same setUI path local Shift+Device uses (chord.go/
// display.go), plus the separate focused gate that mutes Push3-sourced
// pad/CC input (main.go's Fixed()) and this hack's own audio output
// (audiosession.go's run loop) while defocused. Not gated on hubPresent —
// if something POSTs here, honor it regardless of the startup probe.
func (ws *webServer) handleFocus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	setFocused(body.On)
	setUI(ws.pmURL, body.On, ws.params, ws.io, ws.astatus, ws.seq, ws.level)
	w.WriteHeader(http.StatusNoContent)
}

func (ws *webServer) handleIO(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, map[string]any{
		"midi":        ws.io.MIDIOptions(),
		"device":      ws.io.DeviceOptions(),
		"channel":     ws.io.ChannelOptions(),
		"recvChannel": ws.io.RecvChannelOptions(),
	})
}

func (ws *webServer) handleSetIO(set func(int) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Index int `json:"index"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := set(body.Index); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ws.params.MarkDirty()
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSeqStep queues a step toggle — the web UI's equivalent of a SEQ
// page pad press, same ctlToggleStep event main.go's Fixed() builds from
// a real pad.
func (ws *webServer) handleSeqStep(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Track int `json:"track"`
		Step  int `json:"step"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Track < 0 || body.Track >= mmNumTracks || body.Step < 0 || body.Step >= mmNumSteps {
		http.Error(w, "track/step out of range", http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlToggleStep, idx: body.Track, delta: body.Step}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (ws *webServer) handleSeqMute(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Track int `json:"track"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Track < 0 || body.Track >= mmNumTracks {
		http.Error(w, "track out of range", http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlMuteToggle, idx: body.Track}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (ws *webServer) handleSeqPage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Page int `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlStepPage, idx: 1, delta: body.Page}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (ws *webServer) handleTransport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Action != "play" && body.Action != "stop" && body.Action != "record" {
		http.Error(w, "action must be play, stop, or record", http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlTransport, key: body.Action}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleBaseChannel queues an absolute base-channel write — the web UI's
// equivalent of the SEQ page's own BASE CHANNEL encoder (audiosession.go's
// ctlEncoder/pageSeq case), same underlying sharedConfig field, just an
// absolute value instead of a +1/-1 tick.
func (ws *webServer) handleBaseChannel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel int `json:"channel"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlBaseChannel, val: float64(body.Channel)}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleTrack queues a track switch — the web UI's track picker, same
// path as the SETTINGS-adjacent "track" slot the on-screen D-Pad Up/Down
// drives (ctlSetParam already special-cases key=="track" in
// audiosession.go's drainCtl to resync every slider afterward).
func (ws *webServer) handleTrack(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Track int `json:"track"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	select {
	case ws.ctl <- controlEvent{kind: ctlSetParam, key: "track", val: float64(body.Track)}:
	default:
		http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (ws *webServer) handlePage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Page int `json:"page"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	ws.params.setPage(body.Page)
	w.WriteHeader(http.StatusNoContent)
}

func (ws *webServer) handleBank(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bank int `json:"bank"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	setBank(ws.params, body.Bank)
	w.WriteHeader(http.StatusNoContent)
}

func runWebServer(port int, version string, pmURL string, params *paramState, io *ioState, astatus *audioStatus, diag *diagStats,
	seq *seqState, level *levelMeter, ctl chan<- controlEvent, shutdown <-chan struct{}) {

	broker := sse.NewBroker[[]byte](8, false)
	ws := &webServer{version: version, pmURL: pmURL, params: params, io: io, astatus: astatus, diag: diag, seq: seq, level: level, ctl: ctl, broker: broker}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", ws.handleState)
	mux.HandleFunc("GET /api/params", ws.handleParams)
	mux.HandleFunc("GET /sse/state", ws.handleSSE)
	mux.HandleFunc("POST /api/params/{key}", ws.handleSetParam)
	mux.HandleFunc("POST /api/focus", ws.handleFocus)
	mux.HandleFunc("GET /api/io", ws.handleIO)
	mux.HandleFunc("POST /api/io/midi", ws.handleSetIO(ws.io.SetMIDIByIndex))
	mux.HandleFunc("POST /api/io/pcm", ws.handleSetIO(ws.io.SetDeviceByIndex))
	mux.HandleFunc("POST /api/io/channel", ws.handleSetIO(ws.io.SetChannelByIndex))
	mux.HandleFunc("POST /api/io/recv-channel", ws.handleSetIO(ws.io.SetRecvChannelByIndex))
	mux.HandleFunc("POST /api/track", ws.handleTrack)
	mux.HandleFunc("GET /api/presets", ws.handlePresetList)
	mux.HandleFunc("POST /api/presets/save", ws.handlePreset(ctlPresetSave))
	mux.HandleFunc("POST /api/presets/load", ws.handlePreset(ctlPresetLoad))
	mux.HandleFunc("POST /api/page", ws.handlePage)
	mux.HandleFunc("POST /api/bank", ws.handleBank)
	mux.HandleFunc("POST /api/seq/step", ws.handleSeqStep)
	mux.HandleFunc("POST /api/seq/mute", ws.handleSeqMute)
	mux.HandleFunc("POST /api/seq/page", ws.handleSeqPage)
	mux.HandleFunc("POST /api/transport", ws.handleTransport)
	mux.HandleFunc("POST /api/base-channel", ws.handleBaseChannel)
	mux.HandleFunc("/", httpx.ServeEmbedded(webUI, "ui/index.html"))

	handler := httpx.WithLogging(httpx.WithCORS("GET, POST, OPTIONS", mux))
	srv := httpx.NewServer(fmt.Sprintf(":%d", port), handler)

	go func() {
		ticker := time.NewTicker(stateBroadcastInterval)
		defer ticker.Stop()
		for {
			select {
			case <-shutdown:
				return
			case <-ticker.C:
				if data, err := json.Marshal(ws.buildState()); err == nil {
					broker.Broadcast(data)
				}
			}
		}
	}()

	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("web server shutdown: %v", err)
		}
	}()

	log.Printf("web UI listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("web server error: %v", err)
	}
}

const presetExt = ".pumma"

// presetName is the whitelist for preset names: they become file names,
// so no path separators, dots, or anything else that could escape
// presetDir.
var presetName = regexp.MustCompile(`^[A-Za-z0-9 _-]{1,48}$`)

func (ws *webServer) presetDir() string { return filepath.Join(ws.io.HackDir(), "presets") }

func (ws *webServer) handlePresetList(w http.ResponseWriter, r *http.Request) {
	entries, _ := os.ReadDir(ws.presetDir()) // missing dir = no presets yet
	names := []string{}
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), presetExt); ok && presetName.MatchString(n) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	httpx.JSON(w, names)
}

// handlePreset queues a save/load by name; the plugin does the actual
// work off its render thread and reports progress via "preset" in the
// state stream.
func (ws *webServer) handlePreset(kind ctlKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !presetName.MatchString(body.Name) {
			http.Error(w, "bad preset name (1-48 of A-Z a-z 0-9 space _ -)", http.StatusBadRequest)
			return
		}
		if !ws.diag.getReady() {
			http.Error(w, "device booting or loading, try again shortly", http.StatusConflict)
			return
		}
		path := filepath.Join(ws.presetDir(), body.Name+presetExt)
		if kind == ctlPresetSave {
			if err := os.MkdirAll(ws.presetDir(), 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else if _, err := os.Stat(path); err != nil {
			http.Error(w, "no preset "+body.Name, http.StatusNotFound)
			return
		}
		select {
		case ws.ctl <- controlEvent{kind: kind, key: path}:
		default:
			http.Error(w, "control channel full, try again", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}
