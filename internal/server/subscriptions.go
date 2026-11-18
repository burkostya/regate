package server

import (
	"net/http"
	"sync"
	"time"
)

// Long-lived SSE requests are handled by the official MCP SDK, not by the
// legacy Regate JSON-RPC compatibility dispatcher. This process-local guard
// applies before the SDK creates a session or allocates a response stream.
const maxSubscriptionStreams = 64
const maxSubscriptionLifetime = 30 * time.Minute
const subscriptionWriteTimeout = 10 * time.Second

type subscriptionLimiter struct {
	mu     sync.Mutex
	active int
}

func (l *subscriptionLimiter) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= maxSubscriptionStreams {
		return false
	}
	l.active++
	return true
}

func (l *subscriptionLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active == 0 {
		panic("MCP subscription released without successful admission")
	}
	l.active--
}

func (l *subscriptionLimiter) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Keep the SDK's existing SSE framing and flush behavior, but impose a write
// deadline on each outbound event. Unwrap retains net/http ResponseController
// support for SDK methods not overridden here.
type subscriptionDeadlineWriter struct {
	http.ResponseWriter
}

func (w *subscriptionDeadlineWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *subscriptionDeadlineWriter) Write(data []byte) (int, error) {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(subscriptionWriteTimeout))
	return w.ResponseWriter.Write(data)
}

func (w *subscriptionDeadlineWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(subscriptionWriteTimeout))
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
