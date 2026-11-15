package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func sdkFixture(t *testing.T, versions []string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var options *sdk.ServerOptions
	if len(versions) > 0 {
		options = &sdk.ServerOptions{PageSize: 1, SupportedProtocolVersions: versions}
	} else {
		options = &sdk.ServerOptions{PageSize: 1}
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "sdk-upstream-fixture", Version: "1.0"}, options)
	calls := &atomic.Int64{}
	server.AddTool(&sdk.Tool{Name: "echo", Description: "Returns a test payload", InputSchema: map[string]any{"type": "object"}},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			calls.Add(1)
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{
				Content:           []sdk.Content{&sdk.TextContent{Text: "echoed"}},
				StructuredContent: map[string]any{"echo": args},
			}, nil
		})
	server.AddTool(&sdk.Tool{Name: "second", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "second"}}}, nil
		})
	server.AddResource(&sdk.Resource{URI: "test://hello", Name: "hello", MIMEType: "text/plain"},
		func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: "test://hello", MIMEType: "text/plain", Text: "world"}}}, nil
		})
	return httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{Stateless: len(versions) == 0, JSONResponse: true})), calls
}

func TestOfficialSDKModernUpstream(t *testing.T) {
	upstream, calls := sdkFixture(t, nil)
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", 5*time.Second)
	ctx := context.Background()
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("pagination lost tools: %#v", tools)
	}
	result, err := conn.CallTool(ctx, "echo", map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	if result["structuredContent"].(map[string]any)["echo"].(map[string]any)["name"] != "Alice" {
		t.Fatalf("structured content lost: %#v", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected tool executions: %d", calls.Load())
	}
	data, err := conn.Call(ctx, "resources/read", map[string]any{"uri": "test://hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "world") {
		t.Fatalf("resource not proxied: %s", data)
	}
	if got := conn.Status()["protocol"]; got != Protocol {
		t.Fatalf("protocol negotiation failed: %v", got)
	}
}

func TestOfficialSDKLegacyUpstream(t *testing.T) {
	upstream, _ := sdkFixture(t, []string{"2025-06-18"})
	defer upstream.Close()
	conn := NewConn(upstream.URL, "", 5*time.Second)
	result, err := conn.CallTool(context.Background(), "echo", map[string]any{"hello": "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if result["structuredContent"].(map[string]any)["echo"].(map[string]any)["hello"] != "legacy" {
		t.Fatalf("legacy result: %#v", result)
	}
	if got := conn.Status()["protocol"]; got != "2025-06-18" {
		t.Fatalf("legacy negotiation: got %v", got)
	}
}

func TestOfficialSDKAuthorizationAndNoWriteRetry(t *testing.T) {
	t.Setenv("REGATE_TEST_UPSTREAM_TOKEN", "t0ken")
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0ken" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var p struct {
			Method string `json:"method"`
			ID     any    `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad json", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if p.Method == "server/discover" {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": p.ID, "result": map[string]any{"supportedVersions": []string{Protocol}, "capabilities": map[string]any{}}})
			return
		}
		if p.Method == "tools/call" {
			calls.Add(1)
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": p.ID, "result": map[string]any{}})
	}))
	defer upstream.Close()
	conn := NewConn(upstream.URL, "REGATE_TEST_UPSTREAM_TOKEN", 3*time.Second)
	_, err := conn.CallTool(context.Background(), "create", map[string]any{"id": 123})
	if err == nil {
		t.Fatal("lost response must be surfaced as unknown effect, not success")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("non-idempotent call replayed %d times", got)
	}
}

func TestOfficialSDKRejectsNonAllowlistedHTTP(t *testing.T) {
	conn := NewConn("http://10.0.0.1/mcp", "", time.Second)
	_, err := conn.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected outbound security rejection")
	}
}
