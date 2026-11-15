package gjallar

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"yggdrasil/internal/part"
)

// stream is GET /v1/alerts/stream: server-sent events, one `alert` event per
// change, and a comment every 25 s so proxies keep the line open.
//
// It needs the bearer token like every route, so a browser reads it with
// fetch and a ReadableStream, not EventSource (which cannot send headers).
// A listener that falls behind misses changes; on reconnect it reloads
// GET /v1/alerts.
func (g *Gjallar) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		part.Error(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": gjallar\n\n")
	flusher.Flush()

	changes := g.hub.subscribe()
	defer g.hub.unsubscribe(changes)
	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			fmt.Fprint(w, ": keep-alive\n\n")
		case a := <-changes:
			payload, err := json.Marshal(a)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: alert\nid: %d\ndata: %s\n\n", a.ID, payload)
		}
		flusher.Flush()
	}
}
