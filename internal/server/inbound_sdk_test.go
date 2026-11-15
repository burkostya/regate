package server

import (
    "bytes"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func TestIncomingSDKCatalogChangesAfterActivation(t *testing.T) {
    svc, server, _ := fixture(t)
    // Hold the previous endpoint as an in-flight session would: switching
    // revisions must not mutate its published tool descriptors.
    svc.inbound.mu.RLock()
    previous := svc.inbound.public["support"]
    svc.inbound.mu.RUnlock()
    draft, err := svc.Store.DraftCreate()
    if err != nil { t.Fatal(err) }
    active, _ := svc.Current()
    source := strings.Replace(active.Source, `tool "get_customer_direct"`, `tool "crm_lookup"`, 1)
    if source == active.Source { t.Fatal("test fixture has no source tool to rename") }
    if _, err := svc.Store.DraftUpdate(draft.ID, source); err != nil { t.Fatal(err) }
    if _, err := svc.Activate(draft.ID); err != nil { t.Fatal(err) }
    oldRequest := httptest.NewRequest(http.MethodPost,"http://localhost/mcp/support",
        strings.NewReader(`{"jsonrpc":"2.0","id":10,"method":"tools/list","params":{}}`))
    oldRequest.Header.Set("Content-Type","application/json")
    oldRequest.Header.Set("Accept","application/json, text/event-stream")
    oldResponse := httptest.NewRecorder()
    previous.handler.ServeHTTP(oldResponse,oldRequest)
    if oldResponse.Code != http.StatusOK { t.Fatalf("in-flight revision unavailable: HTTP %d: %s",oldResponse.Code,oldResponse.Body.String()) }
    var snapshot map[string]any
    if err := json.Unmarshal(oldResponse.Body.Bytes(),&snapshot); err != nil { t.Fatal(err) }
    retained := false
    for _, entry := range snapshot["result"].(map[string]any)["tools"].([]any) {
        if entry.(map[string]any)["name"]=="get_customer_direct" { retained = true }
    }
    if !retained { t.Fatalf("activation mutated an in-flight SDK catalog: %v",snapshot) }

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
    response := latestCall(t, server.URL+"/mcp/support","", "tools/call",12,
        map[string]any{"name":"get_customer_direct","arguments":map[string]any{}})
    defer response.Body.Close()
    responseBody, err := io.ReadAll(response.Body)
    if err != nil { t.Fatal(err) }
    if response.StatusCode < 400 || !bytes.Contains(responseBody, []byte(`"error"`)) {
        t.Fatalf("obsolete tool remained callable: status=%d body=%s", response.StatusCode, responseBody)
    }
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

func TestIncomingSDKTaskStartRequiresModernHeaders(t *testing.T) {
    _, server, world := fixture(t)
    payload := map[string]any{
        "jsonrpc":"2.0", "id":64, "method":"tools/call",
        "params":map[string]any{
            "name":"summarize_customer",
            "arguments":map[string]any{"customer_id":"C123"},
            "_meta":map[string]any{
                "io.modelcontextprotocol/protocolVersion":"2026-07-28",
                "io.modelcontextprotocol/clientCapabilities":map[string]any{
                    "extensions":map[string]any{"io.modelcontextprotocol/tasks":map[string]any{}},
                },
            },
        },
    }
    body, err := json.Marshal(payload)
    if err != nil { t.Fatal(err) }
    for _, name := range []string{"", "different_tool"} {
        t.Run("name="+name,func(t *testing.T) {
            req, err := http.NewRequest(http.MethodPost, server.URL+"/mcp/support", bytes.NewReader(body))
            if err != nil { t.Fatal(err) }
            req.Header.Set("Content-Type","application/json")
            req.Header.Set("Accept","application/json, text/event-stream")
            req.Header.Set("Mcp-Protocol-Version","2026-07-28")
            req.Header.Set("Mcp-Method","tools/call")
            if name != "" { req.Header.Set("Mcp-Name",name) }
            response, err := http.DefaultClient.Do(req)
            if err != nil { t.Fatal(err) }
            defer response.Body.Close()
            if response.StatusCode != http.StatusBadRequest {
                t.Fatalf("invalid task-start headers: HTTP %d, expected 400",response.StatusCode)
            }
        })
    }
    world.mu.Lock()
    defer world.mu.Unlock()
    if len(world.calls) != 0 { t.Fatalf("MCP tool ran with invalid headers: %v",world.calls) }
}
