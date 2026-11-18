package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func subscriptionCount(b *subscriptionBus) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners)
}

func TestSubscriptionAdmissionIsAtomicAndBounded(t *testing.T) {
	var bus subscriptionBus
	listeners := make([]*subscriptionStream, maxSubscriptionStreams)
	for i := range listeners {
		listeners[i] = &subscriptionStream{
			events: make(chan string, 16),
			notifications: map[string]bool{"notifications/tools/list_changed": true},
		}
		if !bus.add(listeners[i]) {
			t.Fatalf("unexpected refusal at listener %d", i)
		}
	}
	extra := &subscriptionStream{}
	if bus.add(extra) || subscriptionCount(&bus) != maxSubscriptionStreams {
		t.Fatal("over-limit listener was registered")
	}
	if extra.events != nil {
		t.Fatal("rejected listener allocated an event queue")
	}
	bus.emit("notifications/tools/list_changed")
	for i, listener := range listeners {
		select {
		case name := <-listener.events:
			if name != "notifications/tools/list_changed" {
				t.Fatalf("wrong event %d: %q", i, name)
			}
		default:
			t.Fatalf("registered listener %d missed event", i)
		}
	}
	select {
	case <-extra.events:
		t.Fatal("denied listener received event")
	default:
	}
	bus.remove(listeners[0])
	if !bus.add(extra) || subscriptionCount(&bus) != maxSubscriptionStreams {
		t.Fatal("removing a client did not free exactly one slot")
	}
}

func TestSubscriptionCapacityFailsBeforeSSEAck(t *testing.T) {
	svc := &Service{}
	for i := 0; i < maxSubscriptionStreams; i++ {
		if !svc.subscriptions.add(&subscriptionStream{events: make(chan string, 16)}) {
			t.Fatal("failed to fill admission limit")
		}
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp/test", nil)
	svc.listen(w, req, json.RawMessage("17"), map[string]any{
		"notifications": map[string]any{"toolsListChanged": true},
	})
	if w.Code != http.StatusServiceUnavailable ||
		w.Header().Get("Retry-After") != "5" ||
		strings.Contains(w.Body.String(), "acknowledged") ||
		subscriptionCount(&svc.subscriptions) != maxSubscriptionStreams {
		t.Fatalf("overflow must fail before stream, status=%d headers=%v body=%q",
			w.Code, w.Header(), w.Body.String())
	}
}

func TestSubscriptionCancellationReleasesCapacity(t *testing.T) {
	svc := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/mcp/test", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.listen(rec, req, json.RawMessage("23"), map[string]any{
			"notifications": map[string]any{"toolsListChanged": true},
		})
	}()
	seen := false
	for i := 0; i < 100; i++ {
		if subscriptionCount(&svc.subscriptions) == 1 {
			seen = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !seen {
		cancel()
		<-done
		t.Fatal("listener never registered")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled subscription did not release handler")
	}
	if subscriptionCount(&svc.subscriptions) != 0 ||
		rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "notifications/subscriptions/acknowledged") {
		t.Fatalf("subscription did not deregister after cancellation: count=%d code=%d body=%q",
			subscriptionCount(&svc.subscriptions), rec.Code, rec.Body.String())
	}
}
