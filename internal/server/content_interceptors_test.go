package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/netguard"
	"github.com/burkostya/regate/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func contentTestSession(t *testing.T, endpoint, token string) (*sdk.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	client := sdk.NewClient(&sdk.Implementation{Name: "content-contract-test", Version: "1"}, nil)
	httpClient := netguard.Client(5 * time.Second)
	if token != "" {
		httpClient.Transport = tokenRT{next: httpClient.Transport, token: token}
	}
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, ctx
}

func TestPublishedContentInterceptors(t *testing.T) {
	t.Setenv("REGATE_PUBLIC_TOKEN", "")
	t.Setenv("REGATE_ADMIN_TOKEN", "content-admin")
	var resourceReads, promptReads, decisions, observations atomic.Int32
	var decision atomic.Int32
	source := sdk.NewServer(&sdk.Implementation{Name: "content-source", Version: "1"}, nil)
	source.AddResource(&sdk.Resource{Name: "resource", URI: "test://example/document"}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		resourceReads.Add(1)
		return &sdk.ReadResourceResult{
			Contents:  []*sdk.ResourceContents{{URI: "test://example/document", Text: "upstream"}},
			Cacheable: sdk.Cacheable{TTLMs: 60_000, CacheScope: "public"},
		}, nil
	})
	source.AddPrompt(&sdk.Prompt{Name: "provider_prompt"}, func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		promptReads.Add(1)
		return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{
			{Role: "assistant", Content: &sdk.TextContent{Text: req.Params.Arguments["topic"]}},
		}}, nil
	})
	source.AddTool(&sdk.Tool{Name: "clean", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Messages []struct{ Content struct{ Text string } }
		}
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, err
		}
		if len(args.Messages) != 1 {
			return nil, fmt.Errorf("expected one prompt message")
		}
		return &sdk.CallToolResult{StructuredContent: map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "content": map[string]any{"type": "text", "text": "mcp:" + args.Messages[0].Content.Text}},
		}}}, nil
	})
	upstream := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return source }, &sdk.StreamableHTTPOptions{Stateless: true}))
	defer upstream.Close()
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		dec := json.NewDecoder(req.Body)
		var payload struct{ Input map[string]any }
		if err := dec.Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if req.URL.Path == "/observe" {
			observations.Add(1)
			if len(payload.Input) != 3 || payload.Input["status"] == nil || payload.Input["run_id"] == nil {
				t.Error("observer received non-diagnostic input")
			}
			_, _ = w.Write([]byte(`{}`))
			return
		}
		decisions.Add(1)
		if len(payload.Input) != 1 || payload.Input["document"] != "test://example/document" {
			t.Error("decision did not receive configured projection")
		}
		switch decision.Load() {
		case 0:
			_, _ = w.Write([]byte(`{"decision":"allow"}`))
		case 1:
			_, _ = w.Write([]byte(`{"decision":"deny"}`))
		case 2:
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		}
	}))
	defer hooks.Close()

	base := fmt.Sprintf(`
upstream "provider" { url = %q }
hook "approve" {
  transport = "http"
  contract = "decision"
  url = %q
}
hook "observe" {
  transport = "http"
  contract = "observe"
  url = %q
}
hook "clean" {
  transport = "mcp"
  contract = "transform"
  upstream = "provider"
  tool = "clean"
}
policy "document_access" { decision_hook = "approve" }
surface "content" {
  path = "/mcp/content"
  resource "document" {
    uri = "test://example/document"
    source = "provider.document"
  }
  prompt "published" {
    source = "provider.provider_prompt"
    arguments = [{ name = "topic", required = true }]
    input_schema = {
      type = "object"
      properties = {
        arguments = {
          type = "object"
          required = ["topic"]
          additionalProperties = false
          properties = { topic = { type = "string" } }
        }
      }
    }
  }
  prompt "local" { text = "local default" }
}
`, upstream.URL, hooks.URL+"/decision", hooks.URL+"/observe")
	rules := `
interceptor "approve_document" {
  surface = "content"
  resource = "document"
  stage = "on_request"
  policy = "document_access"
  input = "{'document': input.uri}"
}
interceptor "observe_document" {
  surface = "content"
  resource = "document"
  stage = "on_error"
  hook = "observe"
}
interceptor "decorate_document" {
  resource = "document"
  stage = "after_call"
  transform = "{'contents': input.contents.map(item, {'uri': item.uri, 'text': item.text + ' filtered'})}"
}
interceptor "upper_topic" {
  prompt = "published"
  stage = "before_call"
  order = 10
  transform = "{'name': input.name, 'arguments': {'topic': upper(input.arguments.topic)}}"
}
interceptor "validate_topic" {
  prompt = "published"
  stage = "before_call"
  order = 20
  check = "input.arguments.topic == 'REGATE'"
}
interceptor "clean_prompt" {
  prompt = "published"
  stage = "after_call"
  hook = "clean"
}
interceptor "use_request" {
  prompt = "local"
  stage = "before_response"
  transform = "{'messages':[{'role':'user', 'content':{'type':'text', 'text': request.arguments.topic}}]}"
}
`
	svc, err := New(t.TempDir(), base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Store.Close() })
	gateway := httptest.NewServer(svc.Handler())
	t.Cleanup(gateway.Close)
	admin, ctx := contentTestSession(t, gateway.URL+"/mcp/admin", "content-admin")
	client, _ := contentTestSession(t, gateway.URL+"/mcp/content", "")
	_, baseline := svc.Current()
	value, err := svc.Admin(ctx, "regate_draft_create", nil)
	if err != nil {
		t.Fatal(err)
	}
	draft := value.(storage.Draft)
	id := draft.ID
	value, err = svc.Admin(ctx, "regate_draft_patch", map[string]any{"draft_id": id, "source": base + rules})
	if err != nil {
		t.Fatal(err)
	}
	digest := value.(storage.Draft).SourceDigest
	fixtures := map[string]any{
		"source": map[string]any{"contents": []any{map[string]any{"uri": "test://example/document", "text": "fixture"}}},
		"hooks":  map[string]any{"approve_document": map[string]any{"decision": "allow"}},
	}
	params := map[string]any{"draft_id": id, "expected_source_digest": digest, "surface": "content", "resource": "document", "mocks": fixtures}
	reply, err := admin.CallTool(ctx, &sdk.CallToolParams{Name: "regate_test", Arguments: params})
	if err != nil || reply.IsError || !strings.Contains(fmt.Sprint(reply.StructuredContent), "fixture filtered") {
		t.Fatalf("draft test: %v %#v", err, reply)
	}
	if resourceReads.Load() != 0 || decisions.Load() != 0 || observations.Load() != 0 || len(svc.Store.Effects()) != 0 {
		t.Fatal("dry run reached a live dependency or effect journal")
	}
	delete(fixtures, "hooks")
	reply, err = admin.CallTool(ctx, &sdk.CallToolParams{Name: "regate_test", Arguments: params})
	if err != nil || !reply.IsError || decisions.Load() != 0 {
		t.Fatalf("missing hook fixture: %v %#v", err, reply)
	}
	impactResult, err := svc.Admin(ctx, "regate_impact_analyze", map[string]any{"draft_id": id})
	if err != nil {
		t.Fatal(err)
	}
	changes := impactResult.(map[string]any)["content_changes"].([]contentImpactEntry)
	if len(changes) != 3 || changes[2].Name != "document" || !changes[2].After.Stages[0].External {
		t.Fatalf("missing effective content impact: %#v", changes)
	}
	if _, err := svc.Admin(ctx, "regate_activate", map[string]any{"draft_id": id, "expected_source_digest": digest}); err != nil {
		t.Fatal(err)
	}

	catalog, err := client.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, prompt := range catalog.Prompts {
		if prompt.Name == "published" && len(prompt.Arguments) == 1 && prompt.Arguments[0].Required {
			found = true
		}
	}
	if !found {
		t.Fatalf("prompt arguments missing from discovery: %+v", catalog)
	}
	req := &sdk.ReadResourceParams{URI: "test://example/document"}
	resource, err := client.ReadResource(ctx, req)
	if err != nil || resource.Contents[0].Text != "upstream filtered" || resource.TTLMs != 0 || resource.CacheScope != "private" || resourceReads.Load() != 1 {
		t.Fatalf("published resource: %#v, %v", resource, err)
	}
	prompt, err := client.GetPrompt(ctx, &sdk.GetPromptParams{Name: "published", Arguments: map[string]string{"topic": "Regate"}})
	if err != nil || prompt.Messages[0].Content.(*sdk.TextContent).Text != "mcp:REGATE" || promptReads.Load() != 1 {
		t.Fatalf("published prompt: %#v, %v", prompt, err)
	}
	if _, err := client.GetPrompt(ctx, &sdk.GetPromptParams{Name: "published", Arguments: map[string]string{"topic": "denied"}}); err == nil || promptReads.Load() != 1 {
		t.Fatal("CEL denial reached prompt source")
	}
	local, err := client.GetPrompt(ctx, &sdk.GetPromptParams{Name: "local", Arguments: map[string]string{"topic": "argument"}})
	if err != nil || local.Messages[0].Content.(*sdk.TextContent).Text != "argument" {
		t.Fatalf("local prompt: %#v, %v", local, err)
	}
	decision.Store(1)
	if _, err := client.ReadResource(ctx, req); err == nil || resourceReads.Load() != 1 || observations.Load() != 1 {
		t.Fatal("decision denial exposed data or skipped error observer")
	}
	decision.Store(2)
	if _, err := client.ReadResource(ctx, req); err == nil || resourceReads.Load() != 1 || decisions.Load() != 3 || observations.Load() != 2 {
		t.Fatal("lost POST response was replayed or source dispatched")
	}
	unknown := 0
	for _, effect := range svc.Store.Effects() {
		if effect.Status == "unknown" {
			unknown++
		}
	}
	if unknown != 1 {
		t.Fatalf("lost hook outcome was not recorded exactly once: %d", unknown)
	}
	if _, err := svc.Admin(ctx, "regate_rollback", map[string]any{"revision": baseline.ID}); err != nil {
		t.Fatal(err)
	}
	resource, err = client.ReadResource(ctx, req)
	if err != nil || resource.Contents[0].Text != "upstream" || decisions.Load() != 3 {
		t.Fatalf("rollback did not restore the original resource: %#v, %v", resource, err)
	}
}

func TestOAuthContentInterceptorsMaskAndRejectBeforeDispatch(t *testing.T) {
	svc, gateway, _, private := oauthFixture(t)
	cfg, _ := svc.Current()
	var hooks atomic.Int32
	var response atomic.Int32
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hooks.Add(1)
		switch response.Load() {
		case 1:
			_, _ = w.Write([]byte(`{"action":"replace","value":{"messages":[{"role":"assistant","content":{"type":"image","mimeType":"image/png","data":"AQI="}}]}}`))
		case 2:
			_, _ = w.Write([]byte(`{"action":"replace","value":{"messages":[{"role":"bogus","content":{"type":"text","text":"private-response-marker"}}]}}`))
		default:
			_, _ = w.Write([]byte(`{"action":"replace","value":{"messages":[{"role":"assistant","content":{"type":"text","text":"contact test@example.com"}}]}}`))
		}
	}))
	defer external.Close()
	ctx := context.Background()
	d, err := svc.Store.DraftCreateFromRevision(svc.rev.ID, cfg.Source+fmt.Sprintf(`
hook "guarded_transform" {
  transport = "http"
  contract = "transform"
  url = %q
  trust_level = "restricted"
}
interceptor "guarded_prompt" {
  surface = "support"
  prompt = "customer_review"
  stage = "before_response"
  hook = "guarded_transform"
}
`, external.URL))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Activate(d.ID); err != nil {
		t.Fatal(err)
	}
	endpoint := gateway.URL + "/mcp/support"
	token := func(scopes string) string {
		return signedTestJWT(t, private, "https://issuer.test", "https://regate.test/mcp/support", scopes)
	}
	denied, deniedCtx := contentTestSession(t, endpoint, token("surface:read"))
	params := &sdk.GetPromptParams{Name: "customer_review"}
	if _, err = denied.GetPrompt(deniedCtx, params); err == nil || hooks.Load() != 0 {
		t.Fatal("missing object scope reached the hook")
	}
	client, clientCtx := contentTestSession(t, endpoint, token("surface:read docs:read"))
	prompt, err := client.GetPrompt(clientCtx, params)
	if err != nil || prompt.Messages[0].Content.(*sdk.TextContent).Text != "contact [EMAIL_REDACTED]" {
		t.Fatalf("hook-added content must be masked: %#v %v", prompt, err)
	}
	for _, mode := range []int32{1, 2} {
		response.Store(mode)
		if result, err := client.GetPrompt(clientCtx, params); result != nil || err == nil || strings.Contains(err.Error(), "private-response-marker") {
			t.Fatalf("unsafe response escaped (%d): %#v %v", mode, result, err)
		}
	}
	current, rev := svc.Current()
	original := current.Surfaces["support"].Prompts[0]
	out, err := svc.Engine.GetPublishedPrompt(ctx, current, rev.ID, "support", original, nil, &engine.ContentMocks{})
	if err == nil || !strings.Contains(out.Error, "external hook mock required") {
		t.Fatalf("expected explicit dry-run fixture: %+v %v", out, err)
	}
}
