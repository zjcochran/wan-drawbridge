// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Hub fans out change notifications to browsers over Server-Sent Events.
// Notifications are coalesced: a burst of changes sends one event per topic.
type Hub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
	dirty   map[string]bool
	wake    chan struct{}
}

// Topics pages refresh on.
const (
	TopicState     = "state"     // WAN state, checks, runs
	TopicInventory = "inventory" // containers, stacks, policies
)

// NewHub returns a Hub with its delivery goroutine running.
func NewHub() *Hub {
	h := &Hub{clients: map[chan string]struct{}{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
	go h.loop()
	return h
}

// Publish marks a topic changed. It never blocks.
func (h *Hub) Publish(topic string) {
	h.mu.Lock()
	h.dirty[topic] = true
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) loop() {
	for range h.wake {
		time.Sleep(300 * time.Millisecond) // let a burst settle
		h.mu.Lock()
		topics := h.dirty
		h.dirty = map[string]bool{}
		for c := range h.clients {
			for t := range topics {
				select {
				case c <- t:
				default: // slow client: it will catch up on the next change
				}
			}
		}
		h.mu.Unlock()
	}
}

// ServeHTTP streams events until the client goes away.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer

	c := make(chan string, 8)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case t := <-c:
			fmt.Fprintf(w, "event: %s\ndata: %d\n\n", t, time.Now().Unix())
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		}
		flusher.Flush()
	}
}
