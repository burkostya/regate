package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func sessionLossUpstream(t *testing.T, legacy bool, lossMethod string, misses int64, commitBeforeLoss bool) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	attempts, executions := new(atomic.Int64), new(atomic.Int64)
	var lossCandidates atomic.Int64
	var options sdk.ServerOptions
	if legacy {
		options.SupportedProtocolVersions = []string{"2025-11-25"}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "session-loss", Version: "1"}, &options)
	server.AddTool(&sdk.Tool{Name: "lookup", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			executions.Add(1)
			return &sdk.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
		})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: !legacy, JSONResponse: true})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var p struct {
				Method string
				ID     json.RawMessage
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || json.Unmarshal(body, &p) != nil {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			if p.Method == "tools/call" {
				attempts.Add(1)
			}
			if p.Method == lossMethod && lossCandidates.Add(1) <= misses {
				if (r.Header.Get("Mcp-Session-Id") != "") != legacy {
					t.Error("unexpected tool call session header")
				}
				if commitBeforeLoss {
					handler.ServeHTTP(httptest.NewRecorder(), r)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"Invalid session ID"}}`, p.ID)
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream, attempts, executions
}

func TestReadRecoversOnceFromUpstreamSessionLoss(t *testing.T) {
	for _, test := range []struct {
		name                         string
		legacy, retry, pinned        bool
		losses, attempts, executions int64
	}{
		{name: "approved read recovers", legacy: true, retry: true, losses: 1, attempts: 2, executions: 1},
		{name: "pinned read recovers during schema discovery", legacy: true, retry: true, pinned: true, losses: 1, attempts: 1, executions: 1},
		{name: "one recovery even with larger budget", legacy: true, retry: true, losses: 2, attempts: 2},
		{name: "read without approval", legacy: true, losses: 1, attempts: 1},
		{name: "sessionless 404", retry: true, losses: 1, attempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			lossMethod := "tools/call"
			if test.pinned {
				lossMethod = "tools/list"
			}
			upstream, attempts, executions := sessionLossUpstream(t, test.legacy, lossMethod, test.losses, false)
			maxAttempts := 0
			if test.retry {
				maxAttempts = 4
			}
			source := readRetryPipeline(upstream.URL, maxAttempts, 10)
			if test.pinned {
				pins := mcp.ToolSchemaFingerprints([]map[string]any{
					{"name": "lookup", "inputSchema": map[string]any{"type": "object"}},
				})
				source = strings.Replace(source, "timeout_ms = 5000", fmt.Sprintf("timeout_ms = 5000\n  tool_schema_pins = { lookup = %q }", pins["lookup"]), 1)
			}
			cfg, err := config.Decode(source)
			if err != nil {
				t.Fatal(err)
			}
			out, err := New(nil).Run(context.Background(), "rev", cfg, "test", "read", nil, false)
			if attempts.Load() != test.attempts || executions.Load() != test.executions {
				t.Fatalf("attempts=%d executions=%d err=%v", attempts.Load(), executions.Load(), err)
			}
			if test.executions > 0 {
				value, _ := out.Value.(map[string]any)
				if err != nil || out.Status != "success" || value["ok"] != true {
					t.Fatalf("read did not recover: %#v, %v", out, err)
				}
			} else if err == nil || errors.Is(err, mcp.ErrSessionExpired) != test.legacy {
				t.Fatalf("unexpected error class: %v", err)
			}
		})
	}
}

func TestUpstreamSessionLossAfterWriteLeavesUnknownEffect(t *testing.T) {
	upstream, attempts, executions := sessionLossUpstream(t, true, "tools/call", 1, true)
	cfg, err := config.Decode(readRetryPipeline(upstream.URL, 0, 10))
	if err != nil {
		t.Fatal(err)
	}
	store := effectTestStore(t)
	runtime := New(store)
	call := func() error {
		_, err := runtime.mcpCall(context.Background(), cfg, cfg.Upstreams["crm"], "lookup", nil, "run", "write", "write")
		return err
	}
	if err := call(); !errors.Is(err, mcp.ErrSessionExpired) {
		t.Fatalf("write must surface session loss: %v", err)
	}
	effects := store.Effects()
	if len(effects) != 1 || effects[0].Status != "unknown" || effects[0].Reason != mcp.ErrSessionExpired.Error() {
		t.Fatalf("write outcome not recorded as unknown: %+v", effects)
	}
	if err := call(); !errors.Is(err, storage.ErrEffectAlreadyAttempted) {
		t.Fatalf("unknown write became eligible for redispatch: %v", err)
	}
	if attempts.Load() != 1 || executions.Load() != 1 {
		t.Fatalf("attempts=%d executions=%d", attempts.Load(), executions.Load())
	}
}

func TestSessionRecoverySharesReadTimeout(t *testing.T) {
	ctx := withReadRetry(context.Background(), 4, time.Second)
	calls := 0
	_, err := New(nil).callReadWithRetry(ctx, 100*time.Millisecond, func(context.Context) (map[string]any, error) {
		calls++
		return nil, mcp.ErrSessionExpired
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("read attempted recovery beyond its timeout: calls=%d err=%v", calls, err)
	}
}
