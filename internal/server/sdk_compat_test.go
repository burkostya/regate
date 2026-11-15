package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/netguard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Verify that a standard MCP client (not Regate's custom wire test helper)
// can negotiate the latest protocol and consume Regate's public tool surface.
func TestOfficialSDKClientPublicSurfaceInterop(t *testing.T) {
	_, api, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "sdk-compat-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             api.URL + "/mcp/support",
		HTTPClient:           netguard.Client(5 * time.Second),
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().ProtocolVersion; got != mcp.Protocol {
		t.Fatalf("expected latest MCP protocol, got %s", got)
	}
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 2 {
		t.Fatalf("expected 2 published tools: %#v", list.Tools)
	}
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "summarize_customer", Arguments: map[string]any{"customer_id": "C123"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("unexpected MCP error: %#v", result)
	}
}

type tokenRT struct {
	next  http.RoundTripper
	token string
}

func (t tokenRT) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(clone)
}

func TestOfficialSDKClientAdminSurfaceInterop(t *testing.T) {
	_, api, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpClient := netguard.Client(5 * time.Second)
	httpClient.Transport = tokenRT{next: httpClient.Transport, token: "test-secret"}
	client := sdk.NewClient(&sdk.Implementation{Name: "sdk-admin-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             api.URL + "/mcp/admin",
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.InitializeResult().ProtocolVersion; got != mcp.Protocol {
		t.Fatalf("expected latest admin MCP protocol, got %s", got)
	}
	list, err := session.ListTools(ctx, &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list.Tools {
		if tool.Name == "regate_status" {
			found = true
		}
	}
	if !found {
		t.Fatal("admin MCP does not expose regate_status")
	}
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "regate_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("administrative tool failed: %#v", result)
	}
}
