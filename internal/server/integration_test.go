package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type mockWorld struct {
	mu         sync.Mutex
	agentInput map[string]any
	calls      []string
	failWrites bool
}

func (m *mockWorld) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/decision":
		var v struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&v)
		decision := "allow"
		if strings.HasPrefix(v.Input["customer_id"].(string), "DENY") {
			decision = "deny"
		}
		json.NewEncoder(w).Encode(map[string]any{"decision": decision})
		return
	case "/redact":
		var v struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&v)
		v.Input["email"] = "[REDACTED]"
		v.Input["notes"] = "Contact [REDACTED]"
		json.NewEncoder(w).Encode(map[string]any{"action": "replace", "value": v.Input})
		return
	case "/mcp":
		var v struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		m.mu.Lock()
		m.calls = append(m.calls, v.Method)
		m.mu.Unlock()
		result := map[string]any{}
		switch v.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "get_customer", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			if v.Params["name"] == "create" && m.failWrites {
				if hj, ok := w.(http.Hijacker); ok {
					conn, _, e := hj.Hijack()
					if e == nil {
						_ = conn.Close()
					}
				}
				return
			}
			args, _ := v.Params["arguments"].(map[string]any)
			if v.Params["name"] == "get_customer" {
				result = map[string]any{"structuredContent": map[string]any{"customer_id": args["customer_id"], "email": "alice@example.com", "notes": "Contact alice@example.com"}, "content": []any{map[string]any{"type": "text", "text": "raw: alice@example.com"}}}
			} else {
				m.mu.Lock()
				m.agentInput = args
				m.mu.Unlock()
				result = map[string]any{"structuredContent": map[string]any{"safe": true, "summary": args["notes"]}}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": v.ID, "result": result})
		return
	}
	http.NotFound(w, r)
}
func jsonPost(t *testing.T, url, token string, data any) *http.Response {
	t.Helper()
	b, e := json.Marshal(data)
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	return resp
}
func decode(t *testing.T, r *http.Response) map[string]any {
	t.Helper()
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	if e != nil {
		t.Fatal(e)
	}
	var data map[string]any
	if e = json.Unmarshal(b, &data); e != nil {
		t.Fatalf("response status=%d: %s", r.StatusCode, b)
	}
	if r.StatusCode >= 400 {
		t.Fatalf("HTTP %d: %+v", r.StatusCode, data)
	}
	return data
}
func fixture(t *testing.T) (*Service, *httptest.Server, *mockWorld) {
	t.Helper()
	t.Setenv("REGATE_ADMIN_TOKEN", "test-secret")
	world := &mockWorld{}
	mock := httptest.NewServer(world)
	t.Cleanup(mock.Close)
	b, e := os.ReadFile("../../examples/regate.hcl")
	if e != nil {
		t.Fatal(e)
	}
	source := strings.ReplaceAll(string(b), "http://127.0.0.1:9091", mock.URL)
	svc, e := New(t.TempDir(), source)
	if e != nil {
		t.Fatal(e)
	}
	api := httptest.NewServer(svc.Handler())
	t.Cleanup(api.Close)
	return svc, api, world
}
func TestEndToEndMCP(t *testing.T) {
	svc, server, world := fixture(t)
	resp := jsonPost(t, server.URL+"/mcp/support", "", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{}})
	discover := decode(t, resp)
	if discover["error"] != nil {
		t.Fatalf("discover: %v", discover)
	}
	resp = jsonPost(t, server.URL+"/mcp/support", "", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	list := decode(t, resp)
	result := list["result"].(map[string]any)
	if len(result["tools"].([]any)) != 2 {
		t.Fatalf("expected 2 tools, got %+v", result)
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "summarize_customer", "arguments": map[string]any{"customer_id": "C123"}}}
	data := decode(t, jsonPost(t, server.URL+"/mcp/support", "", payload))
	if data["error"] != nil {
		t.Fatal(data)
	}
	content := data["result"].(map[string]any)
	printed, _ := json.Marshal(content)
	if strings.Contains(string(printed), "alice@example.com") {
		t.Fatal("PII leaked in output")
	}
	if content["isError"] == true {
		t.Fatalf("MCP tool failed %+v", content)
	}
	world.mu.Lock()
	agentInput := world.agentInput
	world.mu.Unlock()
	if agentInput["email"] != "[REDACTED]" {
		t.Fatalf("agent received PII: %#v", agentInput)
	}
	if len(svc.Store.Traces()) < 4 {
		t.Fatal("missing node traces")
	}
}
func TestDecisionDenyNoCRMCall(t *testing.T) {
	_, server, world := fixture(t)
	data := decode(t, jsonPost(t, server.URL+"/mcp/support", "", map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "summarize_customer", "arguments": map[string]any{"customer_id": "DENY-1"}}}))
	content := data["result"].(map[string]any)
	if content["isError"] != true {
		t.Fatal("expected denied tool result")
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	for _, m := range world.calls {
		if m == "tools/call" {
			t.Fatal("called CRM despite deny")
		}
	}
}
func TestAdminDraftActivateRollbackAndConflict(t *testing.T) {
	_, server, _ := fixture(t)
	request := func(route string, body any) map[string]any {
		return decode(t, jsonPost(t, server.URL+"/api/"+route, "test-secret", body))
	}
	if resp := jsonPost(t, server.URL+"/api/drafts", "", map[string]any{}); resp.StatusCode != 401 {
		t.Fatalf("admin auth missing: %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	old := request("drafts", map[string]any{})
	second := request("drafts", map[string]any{})
	src := old["source"].(string) + "\n# test revision\n"
	request("draft-update", map[string]any{"draft_id": old["id"], "source": src})
	change := request("activate", map[string]any{"draft_id": old["id"]})
	if change["id"] == old["base"] {
		t.Fatal("activation must allocate new revision ID")
	}
	oldErr := jsonPost(t, server.URL+"/api/activate", "test-secret", map[string]any{"draft_id": second["id"]})
	defer oldErr.Body.Close()
	if oldErr.StatusCode != 400 {
		t.Fatalf("should conflict: HTTP %d", oldErr.StatusCode)
	}
	restore := request("rollback", map[string]any{"revision": old["base"]})
	if restore["id"] == old["base"] {
		t.Fatal("rollback needs new activation identity")
	}
	state := decode(t, jsonPost(t, server.URL+"/mcp/admin", "test-secret", map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "regate_status", "arguments": map[string]any{}}}))
	if state["error"] != nil {
		t.Fatal(state)
	}
}
func TestMCPDirectProxyPreservesContent(t *testing.T) {
	_, server, _ := fixture(t)
	data := decode(t, jsonPost(t, server.URL+"/mcp/support", "", map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": "get_customer_direct", "arguments": map[string]any{"customer_id": "C1"}}}))
	result := data["result"].(map[string]any)
	if len(result["content"].([]any)) != 1 {
		t.Fatal("MCP content not preserved")
	}
	if text := result["content"].([]any)[0].(map[string]any)["text"].(string); text != "raw: alice@example.com" {
		t.Fatalf("modified passthrough content: %s", text)
	}
}
func TestHTTPAdmin(t *testing.T) {
	svc, server, _ := fixture(t)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", server.URL+"/api/catalog", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	data := decode(t, resp)
	if _, ok := data["pipelines"]; !ok {
		t.Fatal("no pipelines")
	}
	if svc.DebugConfig() == "" {
		t.Fatal("empty status")
	}
}

func latestCall(t *testing.T, url, token, method string, id int, extra map[string]any) *http.Response {
	t.Helper()
	params := map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}}
	for k, v := range extra {
		params[k] = v
	}
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if method == "tools/call" || method == "prompts/get" || method == "resources/read" {
		name, _ := params["name"].(string)
		if method == "resources/read" { name, _ = params["uri"].(string) }
		if name != "" { req.Header.Set("Mcp-Name", name) }
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func TestSubscriptionsNotifyOnActivate(t *testing.T) {
	svc, server, _ := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	params := map[string]any{
		"jsonrpc": "2.0", "id": 17, "method": "subscriptions/listen",
		"params": map[string]any{
			"_meta":         map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}},
			"notifications": map[string]any{"toolsListChanged": true},
		},
	}
	raw, _ := json.Marshal(params)
	request, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/mcp/support", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "subscriptions/listen")
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected subscription response: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(res.Body)
	readLine := func() map[string]any {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("SSE data expected: %q", line)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: "))), &v); err != nil {
			t.Fatal(err)
		}
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		return v
	}
	ack := readLine()
	if ack["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatal(ack)
	}
	d, err := svc.Store.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Activate(d.ID); err != nil {
		t.Fatal(err)
	}
	event := readLine()
	if event["method"] != "notifications/tools/list_changed" {
		t.Fatal(event)
	}
	meta := event["params"].(map[string]any)["_meta"].(map[string]any)
	if meta["io.modelcontextprotocol/subscriptionId"] != float64(17) {
		t.Fatalf("wrong subscription ID %v", meta)
	}
}
func TestTaskExecutionAndOwnership(t *testing.T) {
	_, server, _ := fixture(t)
	params := map[string]any{
		"name":      "summarize_customer",
		"arguments": map[string]any{"customer_id": "C456"},
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{
				"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}},
			},
		},
	}
	raw := decode(t, latestCall(t, server.URL+"/mcp/support", "", "tools/call", 17, params))
	result := raw["result"].(map[string]any)
	if result["resultType"] != "task" {
		t.Fatalf("expected task handle: %v", result)
	}
	id := result["taskId"].(string)
	// Task ownership is enforced even when the network request itself is made
	// from an allowed loopback address.
	ownerMismatch := decode(t, latestCall(t, server.URL+"/mcp/support", "other", "tasks/get", 18, map[string]any{"taskId": id}))
	if ownerMismatch["error"] == nil {
		t.Fatal("task visible to other bearer")
	}
	deadline := time.After(2 * time.Second)
	for {
		r := decode(t, latestCall(t, server.URL+"/mcp/support", "", "tasks/get", 19, map[string]any{"taskId": id}))
		if r["error"] != nil {
			t.Fatal(r)
		}
		task := r["result"].(map[string]any)
		if task["status"] == "completed" {
			if task["result"] == nil {
				t.Fatalf("missing task result: %v", task)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("task did not complete: %v", task)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
func TestAdminSkillsAccessible(t *testing.T) {
	_, server, _ := fixture(t)
	d := decode(t, latestCall(t, server.URL+"/mcp/admin", "test-secret", "skills/list", 30, nil))
	if d["error"] != nil {
		t.Fatal(d)
	}
	skills := d["result"].(map[string]any)["skills"].([]any)
	if len(skills) != 2 {
		t.Fatalf("expected two skills, found %v", skills)
	}
	uri := skills[0].(map[string]any)["uri"].(string)
	content := decode(t, latestCall(t, server.URL+"/mcp/admin", "test-secret", "resources/read", 31, map[string]any{"uri": uri}))
	if content["error"] != nil {
		t.Fatal(content)
	}
	body := content["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(body, "Regate") {
		t.Fatalf("invalid skill: %q", body)
	}
}

func TestUnknownEffectJournalAndTraceRestart(t *testing.T) {
	t.Setenv("REGATE_ADMIN_TOKEN", "test-secret")
	world := &mockWorld{failWrites: true}
	mock := httptest.NewServer(world)
	defer mock.Close()
	file, err := os.ReadFile("../../examples/regate.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(file), "http://127.0.0.1:9091", mock.URL)
	source = strings.Replace(source, "  tool \"get_customer_direct\" {", `  tool "write_unknown" {
    source = "crm.create"
    description = "Demonstrate unknown write effect"
  }

  tool "get_customer_direct" {`, 1)
	state := t.TempDir()
	svc, err := New(state, source)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(svc.Handler())
	defer api.Close()
	call := decode(t, jsonPost(t, api.URL+"/mcp/support", "", map[string]any{"jsonrpc": "2.0", "id": 42, "method": "tools/call", "params": map[string]any{"name": "write_unknown", "arguments": map[string]any{"title": "Example"}}}))
	if call["error"] != nil {
		t.Fatal(call)
	}
	if call["result"].(map[string]any)["isError"] != true {
		t.Fatal("expected tool error")
	}
	events := svc.Store.Effects()
	if len(events) != 1 || events[0].Status != "unknown" {
		t.Fatalf("effect journal should preserve unknown: %v", events)
	}
	world.mu.Lock()
	count := 0
	for _, method := range world.calls {
		if method == "tools/call" {
			count++
		}
	}
	world.mu.Unlock()
	if count != 1 {
		t.Fatalf("unexpected automatic retries of write: %d", count)
	}
	first := svc.Store.Traces()
	if len(first) == 0 {
		t.Fatal("no traces before restart")
	}
	restarted, err := New(state, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.Store.Traces()) != len(first) {
		t.Fatal("traces not restored after restart")
	}
	if len(restarted.Store.Effects()) != 1 {
		t.Fatal("effect journal not restored after restart")
	}
}

func TestSurfaceInputSchemaRejectsBeforeExternalEffect(t *testing.T) {
	_, server, world := fixture(t)
	for _, bad := range []map[string]any{{}, {"customer_id": 12}} {
		payload := map[string]any{"jsonrpc": "2.0", "id": 99, "method": "tools/call", "params": map[string]any{"name": "summarize_customer", "arguments": bad}}
		result := decode(t, jsonPost(t, server.URL+"/mcp/support", "", payload))
		if result["error"] == nil {
			content, ok := result["result"].(map[string]any)
			if !ok || content["isError"] != true {
				t.Fatalf("accepted invalid arguments: %#v", result)
			}
		}
	}
	world.mu.Lock()
	defer world.mu.Unlock()
	if len(world.calls) != 0 {
		t.Fatalf("external side effect reached despite invalid input: %v", world.calls)
	}
}

func TestManagementAttributeAddPreservesExistingGraph(t *testing.T) {
	svc, server, _ := fixture(t)
	created := decode(t, jsonPost(t, server.URL+"/api/drafts", "test-secret", map[string]any{}))
	id := created["id"].(string)
	result := decode(t, jsonPost(t, server.URL+"/api/attribute-add", "test-secret", map[string]any{
		"draft_id":   id,
		"path":       "pipeline/customer_summary/node/fetch",
		"attribute":  "input_schema",
		"expression": `{type="object",required=["customer_id"],properties={customer_id={type="string"}}}`,
	}))
	updated, ok := result["source"].(string)
	if !ok || !strings.Contains(updated, "input_schema") {
		t.Fatalf("new attribute missing from draft: %#v", result)
	}
	if !strings.Contains(updated, `node "summarize"`) {
		t.Fatal("existing graph lost")
	}
	draft, err := svc.Store.DraftGet(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Validate(draft.Source); err != nil {
		t.Fatal(err)
	}
	second := jsonPost(t, server.URL+"/api/attribute-add", "test-secret", map[string]any{
		"draft_id": id, "path": "pipeline/customer_summary/node/fetch", "attribute": "input_schema", "expression": `{type="object"}`,
	})
	defer second.Body.Close()
	if second.StatusCode != 400 {
		t.Fatalf("duplicate attribute unexpectedly accepted: HTTP %d", second.StatusCode)
	}
}
