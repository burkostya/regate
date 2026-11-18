package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Use the actual official SDK entrypoint; testing a detached local SSE
// handler alone cannot establish protection for live MCP subscriptions.
func sdkSubscriptionRequest(ctx context.Context, endpoint string, id int) (*http.Request, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id": id,
		"method": "subscriptions/listen",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
			"notifications": map[string]any{"toolsListChanged": true},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	return req, nil
}

func readSubscriptionEvent(reader *bufio.Reader) (map[string]any, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &message); err != nil {
			return nil, fmt.Errorf("malformed SSE notification: %w", err)
		}
		return message, nil
	}
}

func awaitSubscriptions(t *testing.T, svc *Service, count int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if got := svc.subscriptions.count(); got == count {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("subscription count = %d, want %d", svc.subscriptions.count(), count)
}

func TestSubscriptionSDKAdmissionRejectsBeforeDispatch(t *testing.T) {
	svc, api, _ := fixture(t)
	// Reserve the exact process limit directly, without spending 64 sockets.
	for i := 0; i < maxSubscriptionStreams; i++ {
		if !svc.subscriptions.acquire() {
			t.Fatalf("admission refused #%d", i)
		}
	}
	if svc.subscriptions.acquire() {
		t.Fatal("limit admitted an extra subscription")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := sdkSubscriptionRequest(ctx, api.URL+"/mcp/support", 61)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable ||
		resp.Header.Get("Retry-After") != "5" ||
		bytes.Contains(body, []byte("notifications/subscriptions/acknowledged")) ||
		svc.subscriptions.count() != maxSubscriptionStreams {
		t.Fatalf("overflow entered SDK: status %d, headers %v, body %q",
			resp.StatusCode, resp.Header, body)
	}

	svc.subscriptions.release()
	req, err = sdkSubscriptionRequest(ctx, api.URL+"/mcp/support", 62)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("actual SDK stream failed: HTTP %d, %s", resp.StatusCode, b)
	}
	first, err := readSubscriptionEvent(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatal(err)
	}
	if first["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first SDK event was not acknowledgment: %v", first)
	}
	// A closed client must free its capacity even if SDK was waiting for events.
	resp.Body.Close()
	cancel()
	awaitSubscriptions(t, svc, maxSubscriptionStreams-1)
	for i := 0; i < maxSubscriptionStreams-1; i++ {
		svc.subscriptions.release()
	}
}

func TestSubscriptionSDKRealEventDeliveryAndCancellation(t *testing.T) {
	svc, api, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := sdkSubscriptionRequest(ctx, api.URL+"/mcp/support", 71)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("SDK listen failed: HTTP %d, %s", resp.StatusCode, b)
	}
	reader := bufio.NewReader(resp.Body)
	first, err := readSubscriptionEvent(reader)
	if err != nil {
		t.Fatal(err)
	}
	if first["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("SDK did not acknowledge before notification: %v", first)
	}
	meta, _ := first["params"].(map[string]any)["_meta"].(map[string]any)
	if meta["io.modelcontextprotocol/subscriptionId"] != float64(71) {
		t.Fatalf("subscription id mismatch: %v", first)
	}
	awaitSubscriptions(t, svc, 1)
	draft, err := svc.Store.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	active, _ := svc.Current()
	source := strings.Replace(active.Source, `tool "get_customer_direct"», `tool "updated_direct"», 1)
	if source == active.Source {
		t.Fatal("fixture cannot modify tool")
	}
	if _, err := svc.Store.DraftUpdate(draft.ID, source); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Activate(draft.ID); err != nil {
		t.Fatal(err)
	}
	next, err := readSubscriptionEvent(reader)
	if err != nil {
		t.Fatal(err)
	}
	if next["method"] != "notifications/tools/list_changed" {
		t.Fatalf("SDK missed activation event: %v", next)
	}
	cancel()
	resp.Body.Close()
	awaitSubscriptions(t, svc, 0)
}

func TestSubscriptionLimiterReleasesExactCapacity(t *testing.T) {
	var limiter subscriptionLimiter
	for i := 0; i < maxSubscriptionStreams; i++ {
		if !limiter.acquire() {
			t.Fatalf("refused slot %d", i)
		}
	}
	if limiter.acquire() || limiter.count() != maxSubscriptionStreams {
		t.Fatal("overcommitted subscription slots")
	}
	limiter.release()
	if !limiter.acquire() {
		t.Fatal("released slot could not be reused")
	}
	for i := 0; i < maxSubscriptionStreams; i++ {
		limiter.release()
	}
	if limiter.count() != 0 {
		t.Fatal("listener capacity not restored")
	}
}
