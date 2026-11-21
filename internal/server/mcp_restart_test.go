package server

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/netguard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPClientsContinueAfterRegateRestart(t *testing.T) {
	t.Setenv("REGATE_ADMIN_TOKEN", "test-secret")
	source, err := os.ReadFile("../../examples/regate.hcl")
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	start := func(addr string) (*Service, *httptest.Server) {
		t.Helper()
		svc, err := New(state, string(source))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Store.Close() })
		server := httptest.NewUnstartedServer(svc.Handler())
		if addr != "" {
			_ = server.Listener.Close()
			server.Listener, err = net.Listen("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
		}
		server.Start()
		t.Cleanup(server.Close)
		return svc, server
	}
	first, before := start("")
	address := before.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(path, token, version string) *sdk.ClientSession {
		t.Helper()
		httpClient := netguard.Client(3 * time.Second)
		if token != "" {
			httpClient.Transport = tokenRT{next: httpClient.Transport, token: token}
		}
		client := sdk.NewClient(&sdk.Implementation{Name: "restart-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint: before.URL + path, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1,
		}, &sdk.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.Close() })
		return session
	}
	public := connect("/mcp/support", "", "2025-11-25")
	admin := connect("/mcp/admin", "test-secret", "2026-07-28")
	list := func(session *sdk.ClientSession) int {
		t.Helper()
		list, err := session.ListTools(ctx, nil)
		if err != nil || len(list.Tools) == 0 {
			t.Fatalf("could not refresh MCP catalog: %v", err)
		}
		return len(list.Tools)
	}
	publicCount, adminCount := list(public), list(admin)
	subscribe := func(endpoint string, id int) *bufio.Reader {
		t.Helper()
		req, err := sdkSubscriptionRequest(ctx, endpoint, id)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("unexpected subscription response: %s", resp.Status)
		}
		stream := bufio.NewReader(resp.Body)
		if ack, err := readSubscriptionEvent(stream); err != nil || ack["method"] != "notifications/subscriptions/acknowledged" {
			t.Fatalf("subscription %d was not acknowledged: %v %v", id, ack, err)
		}
		return stream
	}
	oldStream := subscribe(before.URL+"/mcp/support", 17)
	before.CloseClientConnections()
	before.Close()
	if _, err := readSubscriptionEvent(oldStream); err == nil {
		t.Fatal("old stream survived shutdown")
	}
	if err := first.Store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, after := start(address)
	if after.URL != before.URL {
		t.Fatalf("restart changed the MCP address: %s", after.URL)
	}
	if list(public) != publicCount || list(admin) != adminCount {
		t.Fatal("catalog changed after reopening the same state")
	}
	stream := subscribe(after.URL+"/mcp/support", 18)
	if list(public) != publicCount {
		t.Fatal("public catalog refresh failed after subscribing again")
	}
	draft, err := restarted.Store.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Activate(draft.ID); err != nil {
		t.Fatal(err)
	}
	if event, err := readSubscriptionEvent(stream); err != nil || event["method"] != "notifications/tools/list_changed" {
		t.Fatalf("new stream did not receive catalog changes: %v %v", event, err)
	}
}
