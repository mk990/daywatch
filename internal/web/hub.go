package web

import (
	"net/http"
	"sync"
	"time"
)

// Hub fans out "new data arrived" signals to connected SSE clients.
type Hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func NewHub() *Hub {
	return &Hub{subs: map[chan struct{}]struct{}{}}
}

// Notify signals all subscribers without blocking the ingest path.
func (h *Hub) Notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // subscriber already has a pending signal
		}
	}
}

func (h *Hub) subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// handleEvents streams server-sent events: an "update" event whenever new
// records are ingested (throttled per client), plus keepalive comments.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	w.Write([]byte("retry: 3000\n\n"))
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	// Throttle so a burst of ingests becomes at most one event per interval.
	throttle := time.NewTicker(2 * time.Second)
	defer throttle.Stop()
	pending := false

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			pending = true
		case <-throttle.C:
			if pending {
				pending = false
				if _, err := w.Write([]byte("event: update\ndata: {}\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		case <-keepalive.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
