package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/storage"
)

const deriveFixture = `# keep this comment and whitespace
pipeline "echo" {
  entry = "result"
  node "result" {
    kind = "output"
    value = "input"
  }
}
surface "tools" {
  path = "/mcp/tools"
  derive_rules = true
  derive_session_headers = ["X-Client-A", "X-Client-B"]
  derive_session_meta_keys = ["client/session"]
  tool "first" {
    pipeline = "echo"
    input_schema = {
      "$defs" = { label = { type = "string" } }
      type = "object"
      required = ["label"]
      properties = { label = { "$ref" = "#/$defs/label" } }
    }
  }
  tool "second" {
    pipeline = "echo"
    input_schema = {
      type = "object"
      required = ["label"]
      properties = { label = { type = "string" } }
    }
  }
}
# trailing comment`

func TestDeriveChainDraftAndActivate(t *testing.T) {
	t.Setenv("REGATE_PUBLIC_TOKEN", "")
	t.Setenv("REGATE_ADMIN_TOKEN", "derive-admin")
	t.Setenv("REGATE_ADMIN_BURST", "100")
	t.Setenv("REGATE_PUBLIC_BURST", "100")
	svc, err := New(t.TempDir(), deriveFixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Store.Close() })
	ctx := context.Background()
	_, rev := svc.Current()
	handler := svc.Handler()
	const explicit = "private-session-498054761"
	const payload = "private-payload-4542291"
	call := func(name, header, metadata string, arguments any) map[string]any {
		params := map[string]any{"name": name, "arguments": arguments}
		if metadata != "" {
			params["_meta"] = map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
				"client/session": metadata,
			}
		}
		req := latestRequest(t, "/mcp/tools", "", "tools/call", 1, params)
		req.RemoteAddr = "127.0.0.1:12345"
		if header != "" {
			req.Header.Set(header, explicit)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return decode(t, w.Result())
	}
	for _, source := range []struct{ header, metadata string }{
		{"X-Client-A", ""}, {"X-Client-B", ""}, {"", explicit}, {"", ""},
	} {
		for _, name := range []string{"first", "second"} {
			reply := call(name, source.header, source.metadata, map[string]any{"label": payload})
			if result, ok := reply["result"].(map[string]any); !ok || result["isError"] == true {
				t.Fatalf("source tool %s failed: %#v", name, reply)
			}
		}
	}
	rows, err := svc.Store.ToolCalls(ctx, "tools", rev.ID)
	if err != nil || len(rows) != 6 {
		t.Fatalf("journal has %d entries: %v", len(rows), err)
	}
	for _, row := range rows {
		if len(row.Group) != 32 || !row.Explicit || !row.Successful {
			t.Fatalf("unexpected metadata for run %s", row.RunID)
		}
	}
	for _, file := range []string{"regate.sqlite", "regate.sqlite-wal"} {
		body, err := os.ReadFile(filepath.Join(svc.Store.Dir, file))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), payload) || strings.Contains(string(body), explicit) {
			t.Fatalf("journal data leaked to %s", file)
		}
	}
	admin := func(name string, args map[string]any) any {
		t.Helper()
		out, err := svc.Admin(ctx, name, args)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	chains := admin("regate_derive_rules", map[string]any{"surface": "tools"}).(chainSearchResult)
	if len(chains.Chains) != 1 || chains.Chains[0].Explicit != 3 || chains.Chains[0].Heuristic != 0 ||
		chains.Chains[0].Steps[0].Tool != "first" || chains.Chains[0].Steps[1].Tool != "second" ||
		len(chains.Chains[0].ExampleRuns) != 3 {
		t.Fatalf("wrong chain evidence: %+v", chains)
	}
	args := map[string]any{
		"surface": "tools", "chain_id": chains.Chains[0].ID,
		"tool_name": "combined", "expected_revision": rev.ID,
	}
	managementRequest := func(token string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(args)
		req := httptest.NewRequest("POST", "/api/derive-draft", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	if w := managementRequest(""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected derive-draft: %d", w.Code)
	}
	args["tool_name"] = "first"
	if w := managementRequest("derive-admin"); w.Code != http.StatusConflict {
		t.Fatalf("name collision: %d %s", w.Code, w.Body)
	}
	args["tool_name"] = "combined"
	w := managementRequest("derive-admin")
	var draft storage.Draft
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &draft) != nil {
		t.Fatalf("draft failed: %d %s", w.Code, w.Body)
	}
	cfg, err := config.Decode(draft.Source)
	if err != nil {
		t.Fatal(err)
	}
	contract := cfg.ToolContracts["tools"]["combined"].Input
	if err := contract.Validate(map[string]any{
		"step_1": map[string]any{"label": 42}, "step_2": map[string]any{"label": "ok"},
	}); err == nil {
		t.Fatal("generated input lost validation through local schema reference")
	}
	originalAt, _ := surfaceInsertion(deriveFixture, "tools")
	suffix := deriveFixture[originalAt:]
	suffixAt := strings.Index(draft.Source, suffix)
	if !strings.HasPrefix(draft.Source, deriveFixture[:originalAt]) || suffixAt < originalAt {
		t.Fatal("generation changed original HCL bytes")
	}
	const pipeline = "derive.tools.combined"
	edited := admin("regate_attribute_patch", map[string]any{
		"draft_id": draft.ID, "expected_source_digest": draft.SourceDigest,
		"path": "pipeline/" + pipeline + "/node/step_2", "attribute": "arguments",
		"expression": `"nodes.step_1.value"`,
	}).(storage.Draft)
	dry := admin("regate_test", map[string]any{
		"draft_id": edited.ID, "expected_source_digest": edited.SourceDigest, "surface": "tools", "tool": "combined",
		"arguments": map[string]any{"step_1": map[string]any{"label": "one"}, "step_2": map[string]any{"label": "two"}},
		"mocks": map[string]any{
			pipeline + ".step_1": map[string]any{"label": "mapped"},
			pipeline + ".step_2": map[string]any{"label": "mocked outcome"},
		},
	}).(engine.Outcome)
	if dry.Status != "success" || !reflect.DeepEqual(dry.Value, map[string]any{"label": "mocked outcome"}) {
		t.Fatalf("mocked run: %+v", dry)
	}
	impact := admin("regate_impact_analyze", map[string]any{"draft_id": edited.ID}).(map[string]any)
	if items := impact["tool_calls"].([]toolCallImpact); len(items) != 2 {
		t.Fatalf("unexpected impact: %+v", items)
	}
	if _, err := svc.ActivateIfDigest(edited.ID, edited.SourceDigest); err != nil {
		t.Fatal(err)
	}
	if w := managementRequest("derive-admin"); w.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d %s", w.Code, w.Body)
	}
	list := httptest.NewRecorder()
	listRequest := latestRequest(t, "/mcp/tools", "", "tools/list", 2, nil)
	listRequest.RemoteAddr = "127.0.0.1:12345"
	handler.ServeHTTP(list, listRequest)
	if !strings.Contains(list.Body.String(), `"name":"combined"`) {
		t.Fatalf("tool missing from catalog: %s", list.Body)
	}
	result := call("combined", "", "", map[string]any{
		"step_1": map[string]any{"label": "original"},
		"step_2": map[string]any{"label": "ignored by mapping"},
	})
	if label := obj(obj(result, "result"), "structuredContent")["label"]; label != "original" {
		t.Fatalf("live mapping failed: %+v", result)
	}
	newChains := admin("regate_derive_rules", map[string]any{"surface": "tools"}).(chainSearchResult)
	if len(newChains.Chains) != 0 || newChains.Revision == rev.ID {
		t.Fatalf("cross-revision evidence: %+v", newChains)
	}
}

func TestDeriveGroupingAndBreaks(t *testing.T) {
	var calls []storage.ToolCallObservation
	at := time.Now().UnixNano()
	add := func(group, name string, start, finish int64, success bool) {
		calls = append(calls, storage.ToolCallObservation{
			Group: group, Tool: name, StartedNS: start, FinishedNS: finish,
			Successful: success, RunID: fmt.Sprint(len(calls)), Explicit: group != "oauth",
		})
	}
	for i := int64(0); i < 3; i++ {
		start := at + i*int64(16*time.Minute)
		add("oauth", "read", start, start+1, true)
		add("oauth", "write", start+2, start+3, true)
		add("oauth", "read", start+4, start+5, true)
		add("oauth", "write", start+6, start+7, true)
	}
	add("s", "read", at, at+1, true)
	add("s", "failure", at+2, at+3, false)
	add("s", "write", at+4, at+5, true)
	add("s", "read", at+6, at+12, true)
	add("s", "write", at+7, at+13, true)
	add("s", "read", at+14, at+15, true)
	add("s", "pending", at+16, 0, false)
	add("s", "write", at+18, at+19, true)
	count := countChains(calls)
	if pair := count[[maxChainSteps]string{"read", "write"}]; pair == nil || pair.Heuristic != 3 || pair.Sessions != 3 {
		t.Fatalf("failed, parallel, pending or duplicate calls corrupted evidence: %+v", pair)
	}
	headers := http.Header{"X-First": {"ambiguous", "also ambiguous"}, "X-Next": {"valid"}}
	surface := config.Surface{DeriveSessionHeaders: []string{"X-First", "X-Next"}, DeriveSessionMetaKeys: []string{"session"}}
	source, value := explicitObservationID(surface, headers, map[string]any{"session": "meta"})
	if source != "header:x-next" || value != "valid" {
		t.Fatalf("did not choose next unique header: %s %s", source, value)
	}
	headers.Set("X-Next", "one,two")
	source, value = explicitObservationID(surface, headers, map[string]any{"session": "meta"})
	if source != "meta:session" || value != "meta" {
		t.Fatalf("did not fall back to metadata: %s %s", source, value)
	}
}

func TestDeriveOAuthFallbackAndOwnerIsolation(t *testing.T) {
	svc, srv, _, private := oauthFixture(t)
	cfg, _ := svc.Current()
	d, err := svc.Store.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	d, err = svc.Store.DraftUpdate(d.ID, strings.Replace(cfg.Source, `surface "support" {`, `surface "support" {
  derive_rules = true
  derive_session_headers = ["X-Id"]`, 1))
	if err != nil {
		t.Fatal(err)
	}
	rev, err := svc.ActivateIfDigest(d.ID, d.SourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"private-user-one", "private-user-two", "private-user-three"} {
		token := signedTestJWTSubject(t, private, "https://issuer.test", "https://regate.test/mcp/support", "surface:read crm:read", user)
		for i, name := range []string{"get_customer_direct", "get_customer_direct", "get_customer_direct", "summarize_customer"} {
			req := latestRequest(t, srv.URL+"/mcp/support", token, "tools/call", 1, map[string]any{
				"name": name, "arguments": map[string]any{"customer_id": "C123"},
			})
			if i < 2 {
				req.Header.Set("X-Id", "shared-explicit-id")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if result := obj(decode(t, resp), "result"); result == nil || result["isError"] == true {
				t.Fatalf("OAuth tool failed: %+v", result)
			}
		}
	}
	rows, err := svc.Store.ToolCalls(context.Background(), "support", rev.ID)
	if err != nil || len(rows) != 12 {
		t.Fatalf("wrong number of authenticated observations: %d %v", len(rows), err)
	}
	groups := map[string]bool{}
	for _, row := range rows {
		groups[row.Group] = row.Explicit
	}
	if len(groups) != 6 {
		t.Fatalf("observation keys crossed owners or sources: %d groups", len(groups))
	}
	current, _ := svc.Current()
	search, err := svc.findCallChains(context.Background(), current, "support", rev.ID, "", "")
	if err != nil || len(search.Chains) != 2 ||
		search.Chains[0].Explicit+search.Chains[1].Explicit != 3 ||
		search.Chains[0].Heuristic+search.Chains[1].Heuristic != 3 {
		t.Fatalf("unexpected OAuth evidence: %+v %v", search, err)
	}
}

func TestToolCallCompileGuards(t *testing.T) {
	surface := config.Surface{Name: "tools", Tools: []config.Tool{{Name: "first"}, {Name: "second"}}}
	source, err := buildCallChainSource(deriveFixture, surface, "combined", "derive.tools.combined",
		[]chainStep{{Tool: "first"}, {Tool: "second"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, source, error string }{
		{"cycle", strings.Replace(source, `tool      = "first"`, `tool      = "combined"`, 1), "cyclic tool.call"},
		{"cross-surface", strings.Replace(source, `surface   = "tools"`, `surface   = "other"`, 1), "not a curated tool"},
		{"suspend", strings.Replace(source, "kind = \"output\"\n    value = \"input\"", `kind = "suspend"`, 1), "tool.call target can suspend"},
		{"duplicate header", strings.Replace(source, `"X-Client-B"`, `"x-client-a"`, 1), "duplicate session header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Decode(tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.error) {
				t.Fatalf("wrong validation error: %v", err)
			}
		})
	}
}

func TestDerivedToolPreservesScopesHooksAndStopsOnUnknownWrite(t *testing.T) {
	svc, srv, world, private := oauthFixture(t)
	var writes atomic.Int64
	var loseResponse atomic.Bool
	writer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var call request
		_ = json.Unmarshal(body, &call)
		if call.Method == "tools/call" && call.Params["name"] == "create" {
			writes.Add(1)
			if loseResponse.Load() {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
					return
				}
				http.Error(w, "response lost", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": call.ID,
				"result": map[string]any{"structuredContent": map[string]any{"created": true}},
			})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		world.ServeHTTP(w, r)
	}))
	t.Cleanup(writer.Close)
	current, _ := svc.Current()
	source := strings.Replace(current.Source, `surface "support" {`, `surface "support" {
  tool "write" {
    source = "writer.create"
    required_scopes = ["crm:write"]
    data_classification = "restricted"
  }`, 1)
	source += fmt.Sprintf(`
upstream "writer" {
  url = %q
  trust_level = "restricted"
}
`, writer.URL+"/mcp")
	cfg, err := config.Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	source, err = buildCallChainSource(source, cfg.Surfaces["support"], "combined", "derive.support.combined", []chainStep{
		{Tool: "summarize_customer", AllowEffects: true},
		{Tool: "write", AllowEffects: true},
		{Tool: "get_customer_direct"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ original, replacement, problem string }{
		{`["crm:read", "crm:write"]`, `["crm:read"]`, "requires scope crm:write"},
		{`allow_effects = true`, `allow_effects = false`, "requires allow_effects"},
	} {
		_, err := config.Decode(strings.Replace(source, tc.original, tc.replacement, 1))
		if err == nil || !strings.Contains(err.Error(), tc.problem) {
			t.Fatalf("compiler accepted unsafe composition: %v", err)
		}
	}
	draft, err := svc.Store.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	draft, err = svc.Store.DraftUpdate(draft.ID, source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateIfDigest(draft.ID, draft.SourceDigest); err != nil {
		t.Fatal(err)
	}
	invoke := func(scopes, customer string) map[string]any {
		t.Helper()
		token := signedTestJWT(t, private, "https://issuer.test", "https://regate.test/mcp/support", scopes)
		response := decode(t, latestCall(t, srv.URL+"/mcp/support", token, "tools/call", 1, map[string]any{
			"name": "combined", "arguments": map[string]any{
				"step_1": map[string]any{"customer_id": customer}, "step_2": map[string]any{"customer_id": "write"},
				"step_3": map[string]any{"customer_id": "result"},
			},
		}))
		return obj(response, "result")
	}
	if result := invoke("surface:read crm:read", "C123"); result["isError"] != true || writes.Load() != 0 {
		t.Fatalf("missing scope had effects: %+v, writes %d", result, writes.Load())
	}
	const fullScopes = "surface:read crm:read crm:write"
	if result := invoke(fullScopes, "DENY123"); result["isError"] != true || writes.Load() != 0 {
		t.Fatalf("child decision did not stop chain: %+v, writes %d", result, writes.Load())
	}
	result := invoke(fullScopes, "C123")
	if value := obj(result, "structuredContent"); value["customer_id"] != "result" || value["email"] != "[REDACTED]" {
		t.Fatalf("child output or masking changed: %+v", result)
	}
	world.mu.Lock()
	agentEmail := world.agentInput["email"]
	toolsBefore := countToolCalls(world.calls)
	world.mu.Unlock()
	if agentEmail != "[REDACTED]" || writes.Load() != 1 {
		t.Fatalf("child hooks or ordering failed: writes=%d", writes.Load())
	}

	loseResponse.Store(true)
	if result := invoke(fullScopes, "C123"); result["isError"] != true || writes.Load() != 2 {
		t.Fatalf("unknown write was replayed or reported successful: %+v, writes %d", result, writes.Load())
	}
	world.mu.Lock()
	// The failed chain runs the summary's CRM read and agent hook. The
	// transport loss in the writer prevents the final CRM read.
	toolsAfter := countToolCalls(world.calls)
	world.mu.Unlock()
	if toolsAfter-toolsBefore != 2 {
		t.Fatalf("chain continued after an uncertain write: %d additional calls", toolsAfter-toolsBefore)
	}
	unknown := false
	for _, effect := range svc.Store.Effects() {
		unknown = unknown || effect.Status == "unknown" && effect.Node == "proxy"
	}
	if !unknown {
		t.Fatal("original writer did not journal an unknown effect")
	}
}

func countToolCalls(methods []string) int {
	count := 0
	for _, method := range methods {
		if method == "tools/call" {
			count++
		}
	}
	return count
}

func TestDeriveShowsSuspendedToolAfterSuccessfulContinuation(t *testing.T) {
	t.Setenv("REGATE_PUBLIC_TOKEN", "")
	source := strings.Replace(deriveFixture, `  tool "first" {`, `  tool "approve" {
    pipeline = "approval"
  }
  tool "first" {`, 1) + `

pipeline "approval" {
  entry = "wait"
  node "wait" {
    kind = "suspend"
  }
  node "finish" {
    kind = "output"
    value = "nodes.wait.value"
  }
  edge {
    from = "wait"
    to = "finish"
  }
}
`
	svc, err := New(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Store.Close() })
	handler := svc.Handler()
	invoke := func(session, name string, args map[string]any, task bool) map[string]any {
		t.Helper()
		params := map[string]any{"name": name, "arguments": args}
		if task {
			params["_meta"] = map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{
					"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}},
				},
			}
		}
		req := latestRequest(t, "/mcp/tools", "", "tools/call", 1, params)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("X-Client-A", session)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		result := obj(decode(t, w.Result()), "result")
		if result == nil || result["isError"] == true {
			t.Fatalf("call %s failed: %s", name, w.Body)
		}
		return result
	}
	for _, session := range []string{"one", "two", "three"} {
		id := str(invoke(session, "approve", map[string]any{}, true), "taskId")
		waitTaskRecoveryState(t, svc, id, "input_required", "")
		if _, err := svc.TaskUpdate(id, "", map[string]any{
			"resume": map[string]any{"action": "accept", "content": map[string]any{"approved": true}},
		}); err != nil {
			t.Fatal(err)
		}
		waitTaskRecoveryState(t, svc, id, "completed", "")
		invoke(session, "second", map[string]any{"label": "after approval"}, false)
	}
	cfg, rev := svc.Current()
	result, err := svc.findCallChains(context.Background(), cfg, "tools", rev.ID, "", "")
	if err != nil || len(result.Chains) != 1 || result.Chains[0].Steps[0].Tool != "approve" ||
		!strings.Contains(result.Chains[0].DraftUnavailable, "suspend") {
		t.Fatalf("unexpected suspension evidence: %+v %v", result, err)
	}
}
