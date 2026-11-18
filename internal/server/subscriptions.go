package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// subscriptionStream represents an HTTP response awaiting a subset of
// MCP notifications. It is intentionally local to one gateway process.
// Prevent an unbounded number of long-lived authenticated SSE handlers from
// pinning goroutines, sockets and event queues. Stateless listeners are
// deliberately evicted after a fixed period and must reconnect and refresh.
const maxSubscriptionStreams = 64
const maxSubscriptionLifetime = 30 * time.Minute
const subscriptionWriteTimeout = 10 * time.Second

type subscriptionStream struct {
	events        chan string
	notifications map[string]bool
}

type subscriptionBus struct {
	mu        sync.Mutex
	listeners map[*subscriptionStream]struct{}
}

func (b *subscriptionBus) add(s *subscriptionStream) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.listeners) >= maxSubscriptionStreams {
		return false
	}
	if b.listeners == nil {
		b.listeners = make(map[*subscriptionStream]struct{})
	}
	b.listeners[s] = struct{}{}
	return true
}
func (b *subscriptionBus) remove(s *subscriptionStream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.listeners, s)
}
func (b *subscriptionBus) emit(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.listeners {
		if !s.notifications[name] {
			continue
		}
		select {
		case s.events <- name:
		default:
			// A lagging client loses the subscription, not an arbitrary
			// number of unbounded in-memory events. Its next read will
			// receive a full list_changed notification if possible.
			select {
			case <-s.events:
			default:
			}
			select {
			case s.events <- name:
			default:
			}
		}
	}
}

func sendSSE(w http.ResponseWriter, v any) error {
	// A slow or non-reading peer must not retain a handler indefinitely.
	// Some test writers do not support deadlines; real net/http writers do.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(subscriptionWriteTimeout))
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}

// listen implements the stateless 2026-07-28 subscriptions/listen POST stream.
// Only list-change subscriptions are supported: individual resource changes
// must be implemented by an upstream resource-specific watcher in the future.
func (s *Service) listen(w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]any) {
	filters, _ := params["notifications"].(map[string]any)
	names := map[string]string{
		"toolsListChanged":     "notifications/tools/list_changed",
		"promptsListChanged":   "notifications/prompts/list_changed",
		"resourcesListChanged": "notifications/resources/list_changed",
	}
	accepted := map[string]any{}
	notify := map[string]bool{}
	for key, method := range names {
		if enabled, _ := filters[key].(bool); enabled {
			accepted[key] = true
			notify[method] = true
		}
	}
	listener := &subscriptionStream{events: make(chan string, 16), notifications: notify}
	if !s.subscriptions.add(listener) {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "subscription capacity reached", http.StatusServiceUnavailable)
		return
	}
	defer s.subscriptions.remove(listener)
	ctx, cancel := context.WithTimeout(r.Context(), maxSubscriptionLifetime)
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	meta := map[string]any{"io.modelcontextprotocol/subscriptionId": json.RawMessage(id)}
	ack := map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged", "params": map[string]any{"_meta": meta, "notifications": accepted}}
	if sendSSE(w, ack) != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case method := <-listener.events:
			notif := map[string]any{"jsonrpc": "2.0", "method": method, "params": map[string]any{"_meta": meta}}
			if sendSSE(w, notif) != nil {
				return
			}
		}
	}
}
