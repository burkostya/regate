package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const proxyInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"unchanged-client","version":"1"},"capabilities":{"sampling":{}}}}`
const proxyInitialized = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
const proxyList = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
const proxyWrite = `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{}}}`

type proxyFixture struct {
	mode                   string
	epoch, init, ready     atomic.Int64
	receivedWrites, writes atomic.Int64
	pings                  atomic.Int64
	dropWrite, resetOnPing atomic.Bool
}

func (f *proxyFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rpc proxyEnvelope
	if r.Method == http.MethodPost {
		if json.NewDecoder(r.Body).Decode(&rpc) != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
	}
	epoch := f.epoch.Load()
	if rpc.Method == "initialize" {
		if r.Header.Get(proxySessionHeader) != "" {
			http.Error(w, "unexpected session on initialize", http.StatusBadRequest)
			return
		}
		w.Header().Set(proxySessionHeader, fmt.Sprintf("backend-%d-%d", epoch, f.init.Add(1)))
		proxyTestRPC(w, rpc.ID, `{"protocolVersion":"2025-11-25","serverInfo":{"name":"upstream","version":"1"},"capabilities":{"tools":{}}}`)
		return
	}
	if rpc.Method == "tools/call" {
		f.receivedWrites.Add(1)
	}
	if !strings.HasPrefix(r.Header.Get(proxySessionHeader), fmt.Sprintf("backend-%d-", epoch)) {
		switch f.mode {
		case "plain":
			http.Error(w, "Bad Request: Invalid session ID", http.StatusBadRequest)
		case "json400", "json200":
			w.Header().Set("Content-Type", "application/json")
			if f.mode == "json400" {
				w.WriteHeader(http.StatusBadRequest)
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid session ID"}}`)
		case "sse200":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32600,\"message\":\"Invalid session ID\"}}\n\n")
		default:
			http.NotFound(w, r)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Seen-Cursor", r.Header.Get("Last-Event-ID"))
		_, _ = io.WriteString(w, "id: 42\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/resources/list_changed\"}\r\n\r\n")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		switch rpc.Method {
		case "notifications/initialized":
			f.ready.Add(1)
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			f.pings.Add(1)
			if f.resetOnPing.CompareAndSwap(true, false) {
				f.epoch.Add(1)
			}
			proxyTestRPC(w, rpc.ID, "{}")
		case "tools/call":
			f.writes.Add(1)
			if f.dropWrite.CompareAndSwap(true, false) {
				conn, _, err := http.NewResponseController(w).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			proxyTestRPC(w, rpc.ID, `{"content":[{"type":"text","text":"Invalid session ID"}]}`)
		case "":
			w.WriteHeader(http.StatusAccepted)
		default:
			proxyTestRPC(w, rpc.ID, `{"items":["upstream"],"nextCursor":"opaque"}`)
		}
	}
}

func proxyTestRPC(w http.ResponseWriter, id json.RawMessage, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
}

func proxyTestConfig(url string) string {
	return fmt.Sprintf(`
upstream "source" {
  url = %q
}
surface "edge" {
  upstream = "source"
  auth = "anonymous"
}
`, url)
}

func proxyTestGateway(t *testing.T, source string) (*Service, *httptest.Server) {
	t.Helper()
	s, err := New(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Store.Close() })
	gateway := httptest.NewServer(s.Handler())
	t.Cleanup(gateway.Close)
	return s, gateway
}

func proxyTestRequest(ctx context.Context, endpoint, method, body, session, token string) (*http.Response, error) {
	r, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if session != "" {
		r.Header.Set(proxySessionHeader, session)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(r)
}

func proxyTestPost(t *testing.T, endpoint, body, session, token string, status int) (http.Header, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := proxyTestRequest(ctx, endpoint, http.MethodPost, body, session, token)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil || r.StatusCode != status {
		t.Fatalf("status %d want %d, read error %v, response %s", r.StatusCode, status, err, raw)
	}
	return r.Header, string(raw)
}

func proxyTestStartSession(t *testing.T, endpoint, token string) (string, string) {
	t.Helper()
	headers, initial := proxyTestPost(t, endpoint, proxyInitialize, "", token, http.StatusOK)
	id := headers.Get(proxySessionHeader)
	if id == "" {
		t.Fatal("missing session")
	}
	proxyTestPost(t, endpoint, proxyInitialized, id, token, http.StatusAccepted)
	return id, initial
}

func TestPublishedMCPRestoresSessionAfterUpstreamRestart(t *testing.T) {
	for _, mode := range []string{"404", "plain", "json400", "json200", "sse200"} {
		t.Run(mode, func(t *testing.T) {
			fixture := &proxyFixture{mode: mode}
			testUpstream := httptest.NewServer(fixture)
			t.Cleanup(func() { testUpstream.Close() })
			_, gateway := proxyTestGateway(t, proxyTestConfig(testUpstream.URL))
			endpoint := gateway.URL + "/mcp/edge"
			directID, directInitialize := proxyTestStartSession(t, testUpstream.URL, "")
			_, directList := proxyTestPost(t, testUpstream.URL, proxyList, directID, "", http.StatusOK)
			id, gatewayInitialize := proxyTestStartSession(t, endpoint, "")
			_, gatewayList := proxyTestPost(t, endpoint, proxyList, id, "", http.StatusOK)
			if strings.HasPrefix(id, "backend-") || gatewayInitialize != directInitialize || gatewayList != directList {
				t.Fatal("proxy leaked the backend session or changed the MCP response")
			}

			address := testUpstream.Listener.Addr().String()
			testUpstream.Close()
			fixture.epoch.Add(1)
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			testUpstream = httptest.NewUnstartedServer(fixture)
			_ = testUpstream.Listener.Close()
			testUpstream.Listener = listener
			testUpstream.Start()

			_, reply := proxyTestPost(t, endpoint, proxyWrite, id, "", http.StatusOK)
			if !strings.Contains(reply, "Invalid session ID") || fixture.init.Load() != 3 || fixture.ready.Load() != 3 ||
				fixture.writes.Load() != 1 || fixture.receivedWrites.Load() != 1 {
				t.Fatalf("write did not recover exactly once, init=%d ready=%d attempts=%d writes=%d",
					fixture.init.Load(), fixture.ready.Load(), fixture.receivedWrites.Load(), fixture.writes.Load())
			}
			proxyTestPost(t, endpoint, proxyList, id, "", http.StatusOK)
			if fixture.init.Load() != 3 {
				t.Fatal("text in a tool result invalidated the session")
			}
			if mode == "sse200" {
				fixture.epoch.Add(1)
				if _, got := proxyTestPost(t, endpoint, proxyList, id, "", http.StatusOK); got != directList || fixture.init.Load() != 4 {
					t.Fatal("the first SSE error event was delivered instead of recovering the read")
				}
			}
		})
	}
}

func TestPublishedMCPSharesRecoveryAndNeverReplaysWrite(t *testing.T) {
	fixture := &proxyFixture{mode: "json200"}
	upstream := httptest.NewServer(fixture)
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL))
	endpoint := gateway.URL + "/mcp/edge"
	id, _ := proxyTestStartSession(t, endpoint, "")
	fixture.epoch.Add(1)
	var group sync.WaitGroup
	start := make(chan struct{})
	for range 12 {
		group.Go(func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := proxyTestRequest(ctx, endpoint, http.MethodPost, proxyList, id, "")
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil || res.StatusCode != http.StatusOK || !strings.Contains(string(body), "nextCursor") {
				t.Errorf("catalog recovery returned %d: %s (%v)", res.StatusCode, body, err)
			}
		})
	}
	close(start)
	group.Wait()
	if fixture.init.Load() != 2 || fixture.ready.Load() != 2 {
		t.Fatal("concurrent calls did not share one initialization")
	}

	fixture.dropWrite.Store(true)
	_, body := proxyTestPost(t, endpoint, proxyWrite, id, "", http.StatusBadGateway)
	if !strings.Contains(body, "outcome is unknown") || fixture.writes.Load() != 1 || fixture.receivedWrites.Load() != 1 {
		t.Fatalf("a lost write result was replayed or reported as known: %s", body)
	}
	fixture.resetOnPing.Store(true)
	proxyTestPost(t, endpoint, proxyWrite, id, "", http.StatusBadGateway)
	if fixture.writes.Load() != 1 || fixture.receivedWrites.Load() != 2 {
		t.Fatal("a race between ping and dispatch caused a replay")
	}
	proxyTestPost(t, endpoint, proxyWrite, id, "", http.StatusOK)
	if fixture.writes.Load() != 2 || fixture.receivedWrites.Load() != 3 {
		t.Fatal("the session was not usable after the failed request")
	}
}

func TestPublishedMCPForwardsClientCallbackWithoutPing(t *testing.T) {
	fixture := &proxyFixture{}
	upstream := httptest.NewServer(fixture)
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL))
	endpoint := gateway.URL + "/mcp/edge"
	id, _ := proxyTestStartSession(t, endpoint, "")
	proxyTestPost(t, endpoint, `{"jsonrpc":"2.0","id":"upstream-request","result":{}}`, id, "", http.StatusAccepted)
	if fixture.pings.Load() != 0 {
		t.Fatal("the gateway tried to ping while upstream was awaiting the callback")
	}
}

func TestPublishedMCPStreamsAndKeepsCursorsInTheirGeneration(t *testing.T) {
	fixture := &proxyFixture{}
	upstream := httptest.NewServer(fixture)
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL))
	endpoint := gateway.URL + "/mcp/edge"
	id, _ := proxyTestStartSession(t, endpoint, "")
	stream := func(session, cursor, forwarded string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set(proxySessionHeader, session)
		r.Header.Set("Last-Event-ID", cursor)
		r.Header.Set("Accept", "text/event-stream")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK || res.Header.Get("X-Seen-Cursor") != forwarded {
			t.Fatalf("stream status %d, forwarded cursor %q", res.StatusCode, res.Header.Get("X-Seen-Cursor"))
		}
		reader := bufio.NewReader(res.Body)
		first, err := reader.ReadString('\n')
		data, dataErr := reader.ReadString('\n')
		if err != nil || dataErr != nil || !strings.Contains(data, "notifications/resources/list_changed") {
			t.Fatalf("no live SSE event: %q %q (%v, %v)", first, data, err, dataErr)
		}
		return strings.TrimSpace(strings.TrimPrefix(first, "id: "))
	}
	before := stream(id, "", "")
	if before == "" || stream(id, before, "42") != before {
		t.Fatal("reconnect in the same upstream session did not resume")
	}
	other, _ := proxyTestStartSession(t, endpoint, "")
	if stream(other, before, "") == before {
		t.Fatal("another public session accepted the previous cursor")
	}
	fixture.epoch.Add(1)
	after := stream(id, before, "")
	if before == after || stream(id, after, "42") != after {
		t.Fatal("recovered stream did not isolate the previous session cursor")
	}
}

func TestPublishedMCPStatelessEndpointPreservesClientProtocol(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.URL.Path != "/provider" || r.URL.Query().Get("view") != "complete" ||
			r.Header.Get("Mcp-Protocol-Version") != "2026-07-28" || r.Header.Get(proxySessionHeader) != "" ||
			r.Header.Get("X-Mcp-Extension") != "local" || r.Header.Get("Authorization") != "" ||
			json.NewDecoder(r.Body).Decode(&message) != nil || r.Header.Get("Mcp-Method") != message.Method {
			http.Error(w, "request metadata was changed", http.StatusBadRequest)
			return
		}
		switch message.Method {
		case "server/discover":
			proxyTestRPC(w, message.ID, `{"serverInfo":{"name":"original"},"capabilities":{"extensions":{"provider.test":{}}}}`)
		case "tools/list":
			proxyTestRPC(w, message.ID, `{"tools":[{"name":"original","inputSchema":{"type":"object"}}]}`)
		case "resources/read":
			proxyTestRPC(w, message.ID, `{"contents":[{"uri":"docs://record","mimeType":"text/plain","text":"upstream"}]}`)
		case "prompts/get":
			proxyTestRPC(w, message.ID, `{"messages":[{"role":"user","content":{"type":"text","text":"original prompt"}}]}`)
		default:
			proxyTestRPC(w, message.ID, `{"content":[{"type":"text","text":"as provided"}]}`)
		}
	}))
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL+"/provider"))
	run := func(endpoint, method string) string {
		t.Helper()
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":21,"method":%q,"params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, method)
		req, err := http.NewRequest(http.MethodPost, endpoint+"?view=complete", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", method)
		req.Header.Set("X-Mcp-Extension", "local")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		content, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != http.StatusOK || res.Header.Get(proxySessionHeader) != "" {
			t.Fatalf("%s returned %d: %s (%v)", method, res.StatusCode, content, err)
		}
		return string(content)
	}
	methods := []string{"server/discover", "tools/list", "tools/call", "resources/read", "prompts/get", "provider.test/custom"}
	for _, method := range methods {
		if direct, proxied := run(upstream.URL+"/provider", method), run(gateway.URL+"/mcp/edge", method); direct != proxied {
			t.Fatalf("%s response changed: direct %s, proxied %s", method, direct, proxied)
		}
	}
	if calls.Load() != int32(2*len(methods)) {
		t.Fatal("stateless route introduced an extra upstream operation")
	}
}

func TestPublishedMCPClientMigrationOnlyChangesURL(t *testing.T) {
	provider := sdk.NewServer(&sdk.Implementation{Name: "original", Version: "1"},
		&sdk.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	provider.AddTool(&sdk.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "original response"}}}, nil
		})
	provider.AddResource(&sdk.Resource{URI: "test://hello", Name: "hello"},
		func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: "test://hello", Text: "original resource"}}}, nil
		})
	provider.AddPrompt(&sdk.Prompt{Name: "review"},
		func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "original prompt"}}}}, nil
		})
	upstream := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return provider }, nil))
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL))
	client := sdk.NewClient(&sdk.Implementation{Name: "unchanged-client", Version: "1"}, nil)
	run := func(endpoint string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: endpoint, MaxRetries: -1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		list, err := session.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: "echo"})
		if err != nil || result.IsError {
			t.Fatalf("tools/call: %v, %#v", err, result)
		}
		resource, err := session.ReadResource(ctx, &sdk.ReadResourceParams{URI: "test://hello"})
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := session.GetPrompt(ctx, &sdk.GetPromptParams{Name: "review"})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := json.Marshal([]any{session.InitializeResult(), list, result, resource, prompt})
		if err != nil {
			t.Fatal(err)
		}
		return string(snapshot)
	}
	if direct, proxied := run(upstream.URL), run(gateway.URL+"/mcp/edge"); direct != proxied {
		t.Fatalf("changing the URL changed the SDK results:\ndirect: %s\nproxied: %s", direct, proxied)
	}
}

func TestPublishedMCPBearerAccessAndRouteChanges(t *testing.T) {
	t.Setenv("REGATE_ADMIN_TOKEN", "management-test")
	t.Setenv("REGATE_TEST_PROXY_BEARER", "client-test")
	t.Setenv("REGATE_TEST_UPSTREAM_BEARER", "upstream-test")
	fixture := &proxyFixture{}
	var contacts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacts.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-test" {
			http.Error(w, "wrong upstream authorization", http.StatusForbidden)
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	source := proxyTestConfig(upstream.URL + "/initial")
	source = strings.Replace(source, `  auth = "anonymous"`, "  auth = \"bearer\"\n  bearer_token_env = \"REGATE_TEST_PROXY_BEARER\"", 1)
	source = strings.Replace(source, "  url = ", "  auth_env = \"REGATE_TEST_UPSTREAM_BEARER\"\n  url = ", 1)
	svc, gateway := proxyTestGateway(t, source)
	endpoint := gateway.URL + "/mcp/edge"
	proxyTestPost(t, endpoint, proxyInitialize, "", "", http.StatusUnauthorized)
	proxyTestPost(t, endpoint, proxyInitialize, "", "management-test", http.StatusUnauthorized)
	proxyTestPost(t, gateway.URL+"/mcp/admin", proxyInitialize, "", "client-test", http.StatusUnauthorized)
	proxyTestPost(t, gateway.URL+"/mcp/source", proxyInitialize, "", "client-test", http.StatusNotFound)
	if contacts.Load() != 0 {
		t.Fatal("a rejected route contacted upstream")
	}
	id, _ := proxyTestStartSession(t, endpoint, "client-test")
	_, base := svc.Current()
	activate := func(nextSource string) {
		t.Helper()
		draft, err := svc.Store.DraftCreate()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Store.DraftUpdate(draft.ID, nextSource); err != nil {
			t.Fatal(err)
		}
		review, err := svc.Admin(context.Background(), "regate_impact_analyze", map[string]any{"draft_id": draft.ID})
		if err != nil {
			t.Fatal(err)
		}
		published := review.(map[string]any)["published_upstreams"].([]map[string]string)
		if len(published) != 1 || published[0]["path"] != "/mcp/edge" || published[0]["upstream"] != "source" || published[0]["auth"] != "bearer" {
			t.Fatalf("publication was missing from impact analysis: %+v", published)
		}
		if _, err := svc.Activate(draft.ID); err != nil {
			t.Fatal(err)
		}
	}
	activate(source + "\n# A revision that does not change the public route.\n")
	proxyTestPost(t, endpoint, proxyList, id, "client-test", http.StatusOK)
	if fixture.init.Load() != 1 {
		t.Fatal("an unrelated activation dropped the session")
	}
	activate(strings.Replace(source, "/initial", "/replacement", 1))
	proxyTestPost(t, endpoint, proxyList, id, "client-test", http.StatusNotFound)
	if _, err := svc.Rollback(base.ID); err != nil {
		t.Fatal(err)
	}
	proxyTestPost(t, endpoint, proxyList, id, "client-test", http.StatusNotFound)
	proxyTestStartSession(t, endpoint, "client-test")
}

func TestPublishedMCPOAuthExchangeAndSessionOwnership(t *testing.T) {
	const (
		issuer = "https://issuer.test"
		public = "https://regate.test/mcp/edge"
		target = "https://upstream.test/mcp"
	)
	t.Setenv("REGATE_PROXY_EXCHANGE_ID", "regate")
	t.Setenv("REGATE_PROXY_EXCHANGE_SECRET", "exchange-test")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	alice := signedTestJWTSubject(t, key, issuer, public, "surface:read", "alice")
	bob := signedTestJWTSubject(t, key, issuer, public, "surface:read", "bob")
	delegated := signedTestJWTSubject(t, key, issuer, target, "upstream:read", "alice")
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/jwks":
			jwk := map[string]string{
				"kid": "test-key", "kty": "RSA", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwk}})
		case "/token":
			if user, password, ok := r.BasicAuth(); !ok || user != "regate" || password != "exchange-test" ||
				r.ParseForm() != nil || r.Form.Get("subject_token") != alice || r.Form.Get("audience") != target ||
				r.Form.Get("scope") != "upstream:read" {
				http.Error(w, "wrong exchange", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": delegated, "issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
				"token_type": "Bearer", "expires_in": 60,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	fixture := &proxyFixture{}
	var contacts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacts.Add(1)
		if bearer(r) != delegated {
			http.Error(w, "invalid upstream credential", http.StatusForbidden)
			return
		}
		fixture.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	source := fmt.Sprintf(`
oauth "company" {
  issuer = %q
  audience = %q
  jwks_url = %q
  token_endpoint = %q
  exchange_client_id_env = "REGATE_PROXY_EXCHANGE_ID"
  exchange_client_secret_env = "REGATE_PROXY_EXCHANGE_SECRET"
}

upstream "source" {
  url = %q
  trust_level = "restricted"
  oauth_exchange = "company"
  oauth_audience = %q
  oauth_scopes = ["upstream:read"]
}
surface "edge" {
  upstream = "source"
  auth = "oauth"
  oauth = "company"
  required_scopes = ["surface:read"]
}
`, issuer, public, idp.URL+"/jwks", idp.URL+"/token", upstream.URL, target)
	_, gateway := proxyTestGateway(t, source)
	endpoint := gateway.URL + "/mcp/edge"
	headers, _ := proxyTestPost(t, endpoint, proxyInitialize, "", delegated, http.StatusUnauthorized)
	if !strings.Contains(headers.Get("WWW-Authenticate"), "https://regate.test/.well-known/oauth-protected-resource/mcp/edge") {
		t.Fatal("missing challenge for the Regate resource")
	}
	res, err := http.Get(gateway.URL + "/.well-known/oauth-protected-resource/mcp/edge")
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	err = json.NewDecoder(res.Body).Decode(&metadata)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || metadata["resource"] != public {
		t.Fatalf("invalid protected resource metadata: %d %v (%v)", res.StatusCode, metadata, err)
	}
	id, _ := proxyTestStartSession(t, endpoint, alice)
	proxyTestPost(t, endpoint, proxyList, id, bob, http.StatusNotFound)
	proxyTestPost(t, endpoint, proxyList, id, alice, http.StatusOK)
	if contacts.Load() != 3 {
		t.Fatal("a rejected caller contacted the upstream")
	}
}

func TestPublishedMCPLimitsAndOutboundRedirects(t *testing.T) {
	var received, redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	_, gateway := proxyTestGateway(t, proxyTestConfig(upstream.URL))
	endpoint := gateway.URL + "/mcp/edge"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := io.NopCloser(strings.NewReader(strings.Repeat(" ", int(maxInboundBodyBytes)+1)))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge || received.Load() != 0 {
		t.Fatalf("oversized chunked request returned %d and reached upstream %d times", res.StatusCode, received.Load())
	}
	proxyTestPost(t, endpoint, proxyWrite, "", "", http.StatusBadGateway)
	if received.Load() != 1 || redirected.Load() != 0 {
		t.Fatal("a redirect changed the outbound destination or caused a replay")
	}
}
