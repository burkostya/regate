package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCatalogRestartsPaginationOnSessionLoss(t *testing.T) {
	for _, test := range []struct {
		name      string
		jsonError bool
		discover  bool
		loseAgain bool
	}{
		{name: "tools with text 404"},
		{name: "full catalog with JSON-RPC 404", jsonError: true, discover: true},
		{name: "replacement session is lost too", jsonError: true, discover: true, loseAgain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			newGeneration := func(names ...string) http.Handler {
				s := sdk.NewServer(&sdk.Implementation{Name: "restart-fixture", Version: "1"}, &sdk.ServerOptions{
					PageSize: 1, SupportedProtocolVersions: []string{"2025-11-25"},
				})
				for _, name := range names {
					s.AddTool(&sdk.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
						func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
							return &sdk.CallToolResult{}, nil
						})
				}
				return sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s },
					&sdk.StreamableHTTPOptions{JSONResponse: true})
			}
			first := newGeneration("old-a", "old-b")
			second := newGeneration("new-a", "new-b")
			var restarted atomic.Bool
			var inits, lists, deletes atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					deletes.Add(1)
				}
				if r.Method == http.MethodPost {
					var p struct {
						Method string                  `json:"method"`
						ID     json.RawMessage         `json:"id"`
						Params struct{ Cursor string } `json:"params"`
					}
					body, err := io.ReadAll(r.Body)
					if err != nil || json.Unmarshal(body, &p) != nil {
						t.Error("could not decode MCP request")
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					// The SDK handler needs the full original request body.
					r.Body = io.NopCloser(bytes.NewReader(body))
					switch p.Method {
					case "initialize":
						inits.Add(1)
						if r.Header.Get("Mcp-Session-Id") != "" {
							t.Error("initialize carried a stale session ID")
						}
					case "tools/list":
						call := lists.Add(1)
						if r.Header.Get("Mcp-Session-Id") == "" || (p.Params.Cursor != "") != (call%2 == 0) {
							t.Errorf("unexpected session or cursor on page %d", call)
						}
						if call == 2 || (call == 4 && test.loseAgain) {
							restarted.Store(true)
							if test.jsonError {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(http.StatusNotFound)
								fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"Invalid session ID"}}`, p.ID)
								return
							}
						}
					}
				}
				if restarted.Load() {
					second.ServeHTTP(w, r)
				} else {
					first.ServeHTTP(w, r)
				}
			}))
			defer upstream.Close()
			conn := NewConn(upstream.URL, "", 3*time.Second)
			var tools []map[string]any
			var err error
			if test.discover {
				var catalog Catalog
				catalog, err = conn.DiscoverCatalog(context.Background())
				tools = catalog.Tools
			} else {
				tools, err = conn.ListTools(context.Background())
			}
			if test.loseAgain {
				if !errors.Is(err, ErrSessionExpired) || err.Error() != ErrSessionExpired.Error() ||
					len(tools) != 0 || inits.Load() != 2 || lists.Load() != 4 {
					t.Fatalf("bad repeated loss: tools=%v inits=%d pages=%d err=%v", tools, inits.Load(), lists.Load(), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(tools) != 2 || tools[0]["name"] != "new-a" || tools[1]["name"] != "new-b" ||
				inits.Load() != 2 || lists.Load() != 4 || deletes.Load() != 1 {
				t.Fatalf("bad recovery: tools=%v inits=%d pages=%d deletes=%d", tools, inits.Load(), lists.Load(), deletes.Load())
			}
		})
	}
}
