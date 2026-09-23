package main

// webserver.go — the hub's own tiny HTTP surface: /api/ping is what every
// other hack's chord.go probes at startup to decide whether to stand
// down its local Shift+Device (docs/push-hub-proposal.md's contract item
// 4); /api/hub/state is a read-only dump of the same data the on-screen
// menu shows, mainly useful for checking the hub's state without needing
// to be looking at Push's actual screen. No embedded browser UI -- see
// the proposal doc's "generalizes cleanly" section; the Push hardware
// screen is the picker, this is just a diagnostic window into it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

func handlePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleHubState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hacks":    getStatuses(),
		"menuOpen": hubUIOn(),
		"cursor":   getCursor(),
	})
}

// runWebServer blocks until shutdown fires -- main.go's one blocking call,
// same role watchHWParams plays in every DSP hack's own main().
func runWebServer(port int, shutdown <-chan struct{}) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ping", handlePing)
	mux.HandleFunc("GET /api/hub/state", handleHubState)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("hub web server shutdown: %v", err)
		}
	}()

	log.Printf("push-hub API listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("hub web server error: %v", err)
	}
}
