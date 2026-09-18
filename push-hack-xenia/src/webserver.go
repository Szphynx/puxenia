package main

// webserver.go — optional browser control surface, alongside (not instead
// of) the on-Push-screen UI. Follows the same goroutine-ownership rule as
// every other input source: reads go straight to paramState's/sharedConfig's
// existing mutex-guarded getters (safe from any goroutine, same as
// display.go's ~30fps redraw loop already does); a param write is queued as
// a ctlSetParam controlEvent on ctlCh so the actual bridge_plugin_set_param
// call still happens on audioSession.run's one goroutine (see main.go's
// midiHandler doc and audiosession.go's drainCtl) — never called directly
// from an HTTP handler goroutine. sharedConfig's I/O setters have no such
// constraint (iopage.go's on-screen commit path already calls them from a
// different goroutine than the render loop) and are called directly here
// too.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/httpx"
	"github.com/federico-pepe/ableton-push-hack/core/sse"
)

//go:embed ui/index.html
var webUI embed.FS

// stateBroadcastInterval is how often /sse/state pushes a fresh snapshot —
// fast enough that a param nudged from the on-screen encoders shows up on
// the web UI without perceptible lag, far below the render loop's own
// per-block rate so it costs nothing worth measuring.
const stateBroadcastInterval = 100 * time.Millisecond

// webServer holds the handlers' shared dependencies — no state of its own
// beyond the SSE broker, since paramState/ioState/sharedConfig/audioStatus
// are already the single source of truth the on-screen UI reads too.
type webServer struct {
	version string
	params  *paramState
	io      *ioState
	astatus *audioStatus
	diag    *diagStats
	ctl     chan<- controlEvent
	broker  *sse.Broker[[]byte]
}

func (ws *webServer) buildState() map[string]any {
	ready, msg := ws.astatus.get()
	snap := ws.params.Snapshot()
	return map[string]any{
		"version":      ws.version,
		"page":         snap.Page,
		"pageName":     snap.PageName,
		"presetCursor": snap.PresetCursor,
		// snap.Params is already keyed by param key — the client reads
		// state.params directly as that map, not state.params.params.
		"params": snap.Params,
		"io": map[string]any{
			"midi":        ws.io.MIDIOptions(),
			"device":      ws.io.DeviceOptions(),
			"channel":     ws.io.ChannelOptions(),
			"recvChannel": ws.io.RecvChannelOptions(),
		},
		"audio": map[string]any{"ready": ready, "message": msg},
		// cpuPercent/activeVoices: updated every ~2s by audiosession.go's
		// render loop (see diagStats) -- real headroom numbers for judging
		// whether a heavier future emulation would fit, not a guess from
		// render-time budget alone.
		"diag": map[string]any{"cpuPercent": ws.diag.getCPU(), "activeVoices": ws.diag.getVoices()},
	}
}

func (ws *webServer) handleState(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, ws.buildState())
}

// handleParams serves every param's metadata (name/type/min/max/options) —
// a one-shot fetch since it only changes if the plugin's own chain_params
// changes, unlike buildState's live values.
func (ws *webServer) handleParams(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, map[string]any{
		"params": ws.params.Metas(),
		"pages":  webParamPages(),
	})
}

// handleSSE streams buildState() as it changes. Writes an immediate
// current-state event before handing off to sse.Serve, so a page load
// shows real values without waiting a full broadcast tick — sse.Serve's own
// doc calls this out as the caller's responsibility.
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

// handleSetParam queues an absolute param write. The actual
// bridge_plugin_set_param call (and any clamping) happens later on
// audioSession.run's goroutine (audiosession.go's drainCtl, ctlSetParam
// case) — this handler only validates the key exists and that the request
// body parses, so 202 means "queued," not "applied."
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

// handleIO serves the SETTINGS page's 3 column option lists (MIDI IN,
// AUDIO OUTPUT, AUDIO CHANNEL) — also included in buildState, exposed
// separately since the option lists themselves don't need SSE-rate polling.
func (ws *webServer) handleIO(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, map[string]any{
		"midi":        ws.io.MIDIOptions(),
		"device":      ws.io.DeviceOptions(),
		"channel":     ws.io.ChannelOptions(),
		"recvChannel": ws.io.RecvChannelOptions(),
	})
}

// handleSetIO adapts one of ioState's SetXByIndex commit methods into a
// POST {"index": n} handler — MIDI/device/channel share the same request
// shape and error handling, only the underlying setter differs.
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
		ws.params.MarkDirty() // so the on-screen SETTINGS page (if open) redraws too
		w.WriteHeader(http.StatusNoContent)
	}
}

// runWebServer serves the browser control surface on port until shutdown
// fires. Fire-and-forget from runSupervised, same shape as
// runDependencyWatcher/runDisplayLoop — it dies with the whole process on
// crash/restart, consistent with every other hack exposing a web_ui.
func runWebServer(port int, version string, params *paramState, io *ioState, astatus *audioStatus, diag *diagStats,
	ctl chan<- controlEvent, shutdown <-chan struct{}) {

	broker := sse.NewBroker[[]byte](8, false)
	ws := &webServer{version: version, params: params, io: io, astatus: astatus, diag: diag, ctl: ctl, broker: broker}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", ws.handleState)
	mux.HandleFunc("GET /api/params", ws.handleParams)
	mux.HandleFunc("GET /sse/state", ws.handleSSE)
	mux.HandleFunc("POST /api/params/{key}", ws.handleSetParam)
	mux.HandleFunc("GET /api/io", ws.handleIO)
	mux.HandleFunc("POST /api/io/midi", ws.handleSetIO(ws.io.SetMIDIByIndex))
	mux.HandleFunc("POST /api/io/pcm", ws.handleSetIO(ws.io.SetDeviceByIndex))
	mux.HandleFunc("POST /api/io/channel", ws.handleSetIO(ws.io.SetChannelByIndex))
	mux.HandleFunc("POST /api/io/recv-channel", ws.handleSetIO(ws.io.SetRecvChannelByIndex))
	mux.HandleFunc("/", httpx.ServeEmbedded(webUI, "ui/index.html"))

	handler := httpx.WithLogging(httpx.WithCORS("GET, POST, OPTIONS", mux))
	srv := httpx.NewServer(fmt.Sprintf(":%d", port), handler)

	// Broadcast tick — simpler and more robust than hooking into
	// paramState.dirty (which the display loop already resets on its own
	// cadence): every source of change (encoders, presets, web writes)
	// lands in paramState/sharedConfig regardless of origin, so a plain
	// poll here needs no extra wiring anywhere else.
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
