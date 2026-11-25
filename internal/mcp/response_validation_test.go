package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamMCPJSONResponseRequiresUnambiguousCompleteFrame(t *testing.T) {
	cases := []struct {
		name, body string
		wantError  bool
	}{
		{"valid", `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`, false},
		{"nested duplicate", `{"jsonrpc":"2.0","id":1,"result":{"allow":false,"allow":true}}`, true},
		{"escaped duplicate", `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"is\u0045rror":false}}`, true},
		{"trailing object", `{"jsonrpc":"2.0","id":1}{"jsonrpc":"2.0","id":2}`, true},
		{"invalid UTF-8", string(append([]byte(`{"jsonrpc":"2.0","text":"`), append([]byte{0xff}, []byte(`"}`)...)...)), true},
		{"over response budget", `{"padding":"` + strings.Repeat("x", MaxResponse) + `"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			conn := NewConn(upstream.URL, "", 5*time.Second)
			req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := conn.Client.Do(req)
			if tc.wantError {
				if err == nil {
					resp.Body.Close()
					t.Fatalf("invalid upstream response was accepted")
				}
				if strings.Contains(err.Error(), "padding") ||
					strings.Contains(err.Error(), "isError") {
					t.Fatalf("untrusted upstream contents leaked into diagnostics: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if _, err := io.ReadAll(resp.Body); err != nil {
				t.Fatal(err)
			}
		})
	}
	// A one-way MCP notification can legitimately produce 202 and no JSON
	// body. Do not turn that into a failure just because of its media type.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", time.Second)
	req, _ := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader([]byte("{}")))
	resp, err := conn.Client.Do(req)
	if err != nil {
		t.Fatalf("202 notification response: %v", err)
	}
	resp.Body.Close()
}

func TestOfficialSDKRejectsDuplicateUpstreamToolResult(t *testing.T) {
	var toolCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if len(envelope.ID) == 0 || bytes.Equal(envelope.ID, []byte("null")) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch envelope.Method {
		case "server/discover":
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":["%s"],"capabilities":{}}}`, envelope.ID, Protocol)
		case "tools/call":
			toolCalls.Add(1)
			// A last-wins JSON decoder would interpret this as isError=false.
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"unsafe"}],"isError":true,"isError":false}}`, envelope.ID)
		default:
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, envelope.ID)
		}
	}))
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", 5*time.Second)
	_, err := conn.CallTool(context.Background(), "untrusted", map[string]any{"id": "x"})
	if toolCalls.Load() != 1 {
		t.Fatalf("expected one upstream dispatch before the response was rejected, got %d: %v", toolCalls.Load(), err)
	}
	if err == nil {
		t.Fatal("SDK accepted an ambiguous upstream MCP result")
	}
}
