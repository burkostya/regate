package server

import (
    "bytes"
    "encoding/json"
    "io"
    "net/http"
    "strings"
    "testing"
)

func TestIncomingSDKCatalogChangesAfterActivation(t *testing.T) {
    svc, server, _ := fixture(t)
    draft, err := svc.Store.DraftCreate()
    if err != nil { t.Fatal(err) }
    active, _ := svc.Current()
    source := strings.Replace(active.Source, `tool "get_customer_direct"`, `tool "crm_lookup"`, 1)
    if source == active.Source { t.Fatal("test fixture has no source tool to rename") }
    if _, err := svc.Store.DraftUpdate(draft.ID, source); err != nil { t.Fatal(err) }
    if _, err := svc.Activate(draft.ID); err != nil { t.Fatal(err) }
    data := decode(t, jsonPost(t, server.URL+"/mcp/support", "", map[string]any{
        "jsonrpc":"2.0", "id":11, "method":"tools/list", "params":map[string]any{},
    }))
    result := data["result"].(map[string]any)
    names := map[string]bool{}
    for _, entry := range result["tools"].([]any) {
        names[entry.(map[string]any)["name"].(string)] = true
    }
    if names["get_customer_direct"] || !names["crm_lookup"] {
        t.Fatalf("SDK catalog contains stale entries: %v", names)
    }
    // The SDK, rather than the old dispatcher, rejects an unpublished tool.
    response := decode(t, latestCall(t, server.URL+"/mcp/support","", "tools/call",12,
        map[string]any{"name":"get_customer_direct","arguments":map[string]any{}}))
    if response["error"] == nil { t.Fatalf("obsolete tool remained callable: %v", response) }
}

func TestIncomingSDKRejectsHeaderMismatchWithoutSideEffects(t *testing.T) {
    _, server, world := fixture(t)
    payload := map[string]any{
        "jsonrpc":"2.0", "id":42, "method":"tools/call",
        "params":map[string]any{
            "name":"summarize_customer",
            "arguments":map[string]any{"customer_id":"C123"},
            "_meta":map[string]any{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},
        },
    }
    data, err := json.Marshal(payload)
    if err != nil { t.Fatal(err) }
    req, err := http.NewRequest("POST",server.URL+"/mcp/support",bytes.NewReader(data))
    if err != nil { t.Fatal(err) }
    req.Header.Set("Content-Type","application/json")
    req.Header.Set("Accept","application/json, text/event-stream")
    req.Header.Set("Mcp-Protocol-Version","2026-07-28")
    req.Header.Set("Mcp-Method","tools/list") // Deliberately inconsistent with body.
    resp, err := http.DefaultClient.Do(req)
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    body, err := io.ReadAll(resp.Body)
    if err != nil { t.Fatal(err) }
    if resp.StatusCode < 400 && !bytes.Contains(body,[]byte(`"error"`)) {
        t.Fatalf("SDK accepted forged method header: status=%d body=%s",resp.StatusCode,body)
    }
    world.mu.Lock()
    defer world.mu.Unlock()
    if len(world.calls) != 0 { t.Fatalf("external effect triggered on invalid headers: %v",world.calls) }
}

func TestIncomingSDKAdminRequiresAuthorization(t *testing.T) {
    _, server, _ := fixture(t)
    response := jsonPost(t,server.URL+"/mcp/admin","",map[string]any{
        "jsonrpc":"2.0","id":91,"method":"server/discover","params":map[string]any{},
    })
    defer response.Body.Close()
    if response.StatusCode != http.StatusUnauthorized { t.Fatalf("got HTTP %d, expected 401",response.StatusCode) }
}
