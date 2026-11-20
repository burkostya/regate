package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPinnedToolChecksEntireCatalogBeforeDispatch(t *testing.T) {
	provider := sdk.NewServer(&sdk.Implementation{Name: "schema-pin", Version: "1"},
		&sdk.ServerOptions{PageSize: 1, SupportedProtocolVersions: []string{"2025-06-18"}})
	input := map[string]any{"type": "object"}
	var calls atomic.Int32
	handler := func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil
	}
	provider.AddTool(&sdk.Tool{Name: "earlier_page", InputSchema: input}, handler)
	provider.AddTool(&sdk.Tool{Name: "target", InputSchema: input}, handler)

	var stateMu sync.Mutex
	sessionsWithTarget := make(map[string]bool)
	var mode atomic.Int32
	const duplicateTarget, unavailable = 1, 2
	provider.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			session := req.GetSession().(*sdk.ServerSession).ID()
			if method == "tools/call" {
				stateMu.Lock()
				listed := sessionsWithTarget[session]
				stateMu.Unlock()
				if session == "" || !listed {
					return nil, errors.New("call did not use the session that listed its schema")
				}
			}
			if method == "tools/list" && mode.Load() == unavailable {
				return nil, errors.New("untrusted provider diagnostic")
			}
			result, err := next(ctx, method, req)
			list, ok := result.(*sdk.ListToolsResult)
			if ok && len(list.Tools) == 1 && list.Tools[0].Name == "target" {
				stateMu.Lock()
				sessionsWithTarget[session] = true
				stateMu.Unlock()
				if mode.Load() == duplicateTarget {
					list.Tools = append(list.Tools, list.Tools[0])
				}
			}
			return result, err
		}
	})
	api := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider },
		&sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer api.Close()

	conn := NewConn(api.URL, "", 5*time.Second)
	ctx := context.Background()
	catalog, err := conn.DiscoverCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pin := ToolSchemaFingerprints(catalog.Tools)["target"]
	if len(pin) != 64 {
		t.Fatal("catalog did not return a usable pin")
	}

	admissions := 0
	admit := func(context.Context) error {
		admissions++
		return nil
	}
	if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, admit); err != nil {
		t.Fatal(err)
	}
	denied := errors.New("local admission denied")
	if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, func(context.Context) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("local admission was not enforced: %v", err)
	}

	t.Run("input_change", func(t *testing.T) {
		provider.AddTool(&sdk.Tool{Name: "target", InputSchema: map[string]any{"type": "object", "additionalProperties": false}}, handler)
		if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, admit); err == nil {
			t.Fatal("input schema change was accepted")
		}
		provider.AddTool(&sdk.Tool{Name: "target", InputSchema: input}, handler)
	})
	t.Run("duplicate_name", func(t *testing.T) {
		mode.Store(duplicateTarget)
		defer mode.Store(0)
		if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, admit); err == nil {
			t.Fatal("duplicate tool name was accepted")
		}
	})
	t.Run("unavailable_catalog", func(t *testing.T) {
		mode.Store(unavailable)
		defer mode.Store(0)
		if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, admit); err == nil || strings.Contains(err.Error(), "untrusted") {
			t.Fatalf("expected safe catalog verification refusal, got %v", err)
		}
	})
	t.Run("missing_tool", func(t *testing.T) {
		provider.RemoveTools("target")
		if _, err := conn.CallPinnedTool(ctx, "target", nil, pin, admit); err == nil {
			t.Fatal("missing tool was accepted")
		}
	})
	if calls.Load() != 1 || admissions != 1 {
		t.Fatalf("upstream calls=%d local admissions=%d, want one of each", calls.Load(), admissions)
	}
}
