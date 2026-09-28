package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Neutx/syncthing-v2/internal/model"
)

// ssePing keeps idle streams (and any intermediary) from timing out.
const ssePing = 15 * time.Second

// stateNames are the wire names of model.State values.
var stateNames = map[model.State]string{
	model.StateDown:         "down",
	model.StateUnauthorized: "unauthorized",
	model.StateError:        "error",
	model.StatePaused:       "paused",
	model.StateSyncing:      "syncing",
	model.StateScanning:     "scanning",
	model.StateNoPeer:       "nopeer",
	model.StateInSync:       "insync",
}

// wireSnapshot is the JSON sent to the page: the snapshot's own fields plus
// the state's name, so the page never depends on enum ordinals.
type wireSnapshot struct {
	model.Snapshot
	StateName string
}

func toWire(s model.Snapshot) wireSnapshot {
	name, ok := stateNames[s.State]
	if !ok {
		name = "down"
	}
	return wireSnapshot{Snapshot: s, StateName: name}
}

// subscribe returns the snapshot stream for a page, honouring ?demo=<view>
// on backends that support views.
func (s *Server) subscribe(view string) (<-chan model.Snapshot, func()) {
	if vb, ok := s.b.(ViewBackend); ok && view != "" {
		return vb.SnapshotsFor(view)
	}
	return s.b.Snapshots()
}

// handleState streams snapshots as Server-Sent Events ("event: snapshot").
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.subscribe(r.URL.Query().Get("demo"))
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 2000\n\n"); err != nil {
		return
	}
	fl.Flush()

	ping := time.NewTicker(ssePing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case snap, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(toWire(snap))
			if err != nil {
				return
			}
			// json.Marshal output has no raw newlines, so one data line suffices.
			if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
