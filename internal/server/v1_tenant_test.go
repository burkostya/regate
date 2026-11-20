package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/mcp"
)

// Two trust domains use the same OAuth subject, scopes and record IDs. Their
// distinct issuers and backend addresses are selected by the public surfaces.
func TestV1TenantBoundaries(t *testing.T) {
	const admin = "synthetic-tenant-admin"
	const sharedID = "shared-record-id"
	t.Setenv("REGATE_ADMIN_TOKEN", admin)
	t.Setenv("REGATE_OTLP_TRACES_URL", "")
	template, err := os.ReadFile("../../examples/v1-acceptance.hcl")
	if err != nil {
		t.Fatal(err)
	}
	_, chain, found := strings.Cut(string(template), "oauth \"acceptance\"")
	if !found {
		t.Fatal("acceptance template has no OAuth configuration")
	}
	chain = "oauth \"acceptance\"" + chain
	root := t.TempDir()
	type tenant struct {
		name, path, prefix, email, token, task, source string
		alpha, beta                                    *mcp.Conn
		calls                                          *acceptanceCalls
	}
	tenants := []tenant{
		{name: "tenant-a", path: "/mcp/acceptance", prefix: "Tenant A contact ", email: "tenant-a-marker@example.com"},
		{name: "tenant-b", path: "/mcp/second", prefix: "Tenant B contact ", email: "tenant-b-marker@example.com"},
	}
	for i := range tenants {
		domain := &tenants[i]
		dir := filepath.Join(root, domain.name)
		alpha := startStandProcess(t, "alpha", filepath.Join(dir, "alpha"))
		beta := startStandProcess(t, "beta", filepath.Join(dir, "beta"))
		domain.alpha = mcp.NewConn(alpha.url, "", 5*time.Second)
		domain.beta = mcp.NewConn(beta.url, "", 5*time.Second)
		support, keys, calls := startAcceptanceSupportForNotes(t, domain.prefix, domain.email)
		domain.calls = calls
		issuer, audience := acceptanceIssuer, acceptanceAudience
		domain.source = strings.NewReplacer(
			"http://127.0.0.1:9201", strings.TrimSuffix(alpha.url, "/mcp"),
			"http://127.0.0.1:9202", strings.TrimSuffix(beta.url, "/mcp"),
			"http://127.0.0.1:9203", support.URL,
		).Replace(chain)
		if i == 1 {
			issuer, audience = "https://second-issuer.test", "https://regate.test"+domain.path
			domain.source = strings.NewReplacer(
				acceptanceIssuer, issuer, acceptanceAudience, audience,
				`"/mcp/acceptance"`, `"/mcp/second"`,
				`"acceptance"`, `"second"`,
				`"alpha"`, `"second_alpha"`, `"beta"`, `"second_beta"`,
				`upstream "agent"`, `upstream "second_agent"`,
				`upstream    = "agent"`, `upstream    = "second_agent"`,
				`hook "decision"`, `hook "second_decision"`,
				`decision_hook = "decision"`, `decision_hook = "second_decision"`,
				`"scrubber"`, `"second_scrubber"`, `"summary_agent"`, `"second_summary_agent"`,
				`"publish_permission"`, `"second_publish_permission"`,
				`"beta_record"`, `"second_beta_record"`, `"beta.put_record"`, `"second_beta.put_record"`,
				`pipeline "publish_summary"`, `pipeline "second_publish_summary"`,
				`pipeline            = "publish_summary"`, `pipeline            = "second_publish_summary"`,
			).Replace(domain.source)
		}
		domain.token = signedTestJWTSubject(t, keys, issuer, audience, "acceptance:access summary:write", "shared-subject")
		for id, value := range map[string]string{
			sharedID: domain.prefix + domain.email, domain.name + "-only": domain.prefix + domain.email,
			"scrubber-failure": "Fail scrubber for " + domain.email,
		} {
			acceptanceProviderCall(t, domain.alpha, "put_record", map[string]any{"record_id": id, "value": value})
		}
	}
	source := `regate {
  listen = "127.0.0.1:0"
}
` + tenants[0].source + tenants[1].source
	gateway, err := New(filepath.Join(root, "gateway"), source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gateway.Store.Close() })
	server := httptest.NewServer(gateway.Handler())
	t.Cleanup(server.Close)
	first, second := newSoakProxy(t), newSoakProxy(t)
	for _, proxy := range []*soakProxy{first, second} {
		proxy.directTo(t, &standProcess{url: server.URL + "/mcp"}, nil)
	}
	firstURL, secondURL := v1Ingress(t, first.URL), v1Ingress(t, second.URL)
	private := func(t *testing.T, body []byte) {
		t.Helper()
		for _, domain := range tenants {
			if strings.Contains(string(body), domain.email) || strings.Contains(string(body), domain.token) {
				t.Fatal("response exposed synthetic PII or an access token")
			}
		}
	}
	client := &http.Client{Timeout: 8 * time.Second}
	call := func(t *testing.T, endpoint, token, method string, params map[string]any, want int) map[string]any {
		t.Helper()
		resp, err := client.Do(latestRequest(t, endpoint, token, method, 1, params))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		private(t, raw)
		if want != 0 && want != resp.StatusCode {
			t.Fatalf("HTTP %d, want %d", resp.StatusCode, want)
		}
		var body map[string]any
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			return nil
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	tool := func(id string, approval bool) map[string]any {
		return map[string]any{"name": "publish_summary", "arguments": map[string]any{
			"record_id": id, "lose_reply": false, "approval_required": approval,
		}}
	}
	for i := range tenants {
		domain := &tenants[i]
		params := tool(sharedID, true)
		params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{
				"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}},
			}}
		reply := call(t, firstURL+domain.path, domain.token, "tools/call", params, http.StatusOK)
		result, _ := reply["result"].(map[string]any)
		domain.task, _ = result["taskId"].(string)
		if domain.task == "" {
			t.Fatal("no task handle for the tenant")
		}
		waitAcceptanceTask(t, firstURL+domain.path, domain.token, domain.task, "input_required")
	}
	for i, active := range tenants {
		other := tenants[1-i]
		t.Run(active.name, func(t *testing.T) {
			t.Parallel()
			endpoint := secondURL + active.path
			call(t, endpoint, other.token, "tools/call", tool(sharedID, false), http.StatusUnauthorized)
			for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
				params := map[string]any{"taskId": other.task}
				if method == "tasks/update" {
					params["inputResponses"] = map[string]any{"resume": map[string]any{"action": "accept", "content": true}}
				}
				reply := call(t, endpoint, active.token, method, params, 0)
				failure, _ := reply["error"].(map[string]any)
				if failure["message"] != "task not found" {
					t.Fatalf("%s did not refuse the foreign task", method)
				}
			}
			for _, id := range []string{other.name + "-only", "DENY-tenant", "scrubber-failure"} {
				reply := call(t, endpoint, active.token, "tools/call", tool(id, false), http.StatusOK)
				result, _ := reply["result"].(map[string]any)
				if result["isError"] != true || result["structuredContent"] != nil {
					t.Fatal("refused tenant call returned data")
				}
			}
			reply := call(t, endpoint, active.token, "tools/call", tool(sharedID, false), http.StatusOK)
			result, _ := reply["result"].(map[string]any)
			if result["isError"] == true || standContent(t, result)["record_id"] != sharedID {
				t.Fatal("tenant publish failed")
			}
			record := acceptanceProviderCall(t, active.beta, "get_record", map[string]any{"record_id": sharedID})
			assertRecord(t, record, "beta", sharedID, "Summary for "+sharedID+": "+active.prefix+"[REDACTED]")
			assertStandCount(t, acceptanceProviderCall(t, active.beta, "stats", nil), "writes_received", 1)
			if active.calls.agent.Load() != 1 {
				t.Fatal("refused input reached a tenant agent")
			}
			waitAcceptanceTask(t, endpoint, active.token, active.task, "input_required")
		})
	}
	// Run after the parallel tenant subtests so all diagnostics are present.
	t.Cleanup(func() {
		for _, path := range []string{"traces", "effects", "audit", "metrics"} {
			var raw json.RawMessage
			acceptanceAdminGet(t, firstURL+"/api/"+path, admin, tenants[0].token, &raw)
			private(t, raw)
		}
	})
}
