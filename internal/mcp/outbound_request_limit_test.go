package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamMCPOutgoingHTTPBodyBudget(t *testing.T) {
	var requests atomic.Int64
	var lastBytes atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read HTTP request: %v", err)
		}
		lastBytes.Store(int64(len(data)))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", 5*time.Second)
	call := func(size int) error {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			upstream.URL, bytes.NewReader(bytes.Repeat([]byte("a"), size)))
		if err != nil {
			t.Fatal(err)
		}
		response, err := conn.Client.Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	if err := call(MaxRequest); err != nil {
		t.Fatalf("exactly %d bytes must be accepted: %v", MaxRequest, err)
	}
	if requests.Load() != 1 || lastBytes.Load() != MaxRequest {
		t.Fatalf("exact-limit body not fully delivered: calls=%d bytes=%d", requests.Load(), lastBytes.Load())
	}
	if err := call(MaxRequest+1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized request was not rejected: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("oversized MCP request reached upstream: %d requests", requests.Load())
	}
}

func TestOfficialSDKRejectsOversizedToolInputWithoutRemoteEffect(t *testing.T) {
	upstream, calls := sdkFixture(t, nil)
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", 5*time.Second)
	// The encoded JSON-RPC envelope must exceed MaxRequest even though
	// every transport handshake stays small and succeeds normally.
	_, err := conn.CallTool(context.Background(), "echo",
		map[string]any{"payload": strings.Repeat("x", MaxRequest)})
	if err == nil {
		t.Fatal("oversized SDK-generated MCP request succeeded")
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized SDK request invoked remote tool %d times", calls.Load())
	}
}
