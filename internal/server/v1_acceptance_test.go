package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	acceptanceIssuer       = "https://issuer.test"
	acceptanceAudience     = "https://regate.test/mcp/acceptance"
	acceptanceEmail        = "acceptance-marker@example.com"
	acceptanceNotes        = "Contact " + acceptanceEmail
	acceptanceRedacted     = "Contact [REDACTED]"
	acceptanceScrubFailure = "Fail scrubber for " + acceptanceEmail
)

// The allow, fault and refusal cases use one OAuth-protected graph and
// separate alpha and beta SDK processes.
func TestV1Acceptance(t *testing.T) {
	const adminToken = "v1-acceptance-local-admin"
	t.Setenv("REGATE_ADMIN_TOKEN", adminToken)
	t.Setenv("REGATE_OTLP_TRACES_URL", "")

	root := t.TempDir()
	alpha := startStandProcess(t, "alpha", filepath.Join(root, "alpha"))
	beta := startStandProcess(t, "beta", filepath.Join(root, "beta"))
	support, private, calls := startAcceptanceSupport(t)
	oauth := newV1OAuthFixture(t, acceptanceIssuer, acceptanceAudience, support.URL+"/jwks", "regate-primary", private)
	token := oauth.mint(t, "acceptance:access summary:write", "test-user")

	template, err := os.ReadFile("../../examples/v1-acceptance.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"http://127.0.0.1:9201", strings.TrimSuffix(alpha.url, "/mcp"),
		"http://127.0.0.1:9202", strings.TrimSuffix(beta.url, "/mcp"),
		"http://127.0.0.1:9203/jwks", oauth.jwks,
		"http://127.0.0.1:9203", support.URL,
		acceptanceIssuer, oauth.issuer, acceptanceAudience, oauth.audience,
	).Replace(string(template))

	state := filepath.Join(root, "gateway")
	startGateway := func(t *testing.T) (string, string, func()) {
		t.Helper()
		svc, err := New(state, source)
		if err != nil {
			t.Fatal(err)
		}
		api := httptest.NewServer(svc.Handler())
		_, rev := svc.Current()
		closed := false
		closeGateway := func() {
			if !closed {
				closed = true
				api.Close()
				if err := svc.Store.Close(); err != nil {
					t.Errorf("close gateway state: %v", err)
				}
			}
		}
		t.Cleanup(closeGateway)
		return v1Ingress(t, api.URL), rev.ID, closeGateway
	}

	alphaConn := mcp.NewConn(alpha.url, "", 5*time.Second)
	betaConn := mcp.NewConn(beta.url, "", 5*time.Second)
	providerCall := acceptanceProviderCall

	for _, tc := range []struct {
		name, status string
		loseReply    bool
	}{
		{name: "confirmed", status: "confirmed"},
		{name: "lost_reply", status: "unknown", loseReply: true},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			providerCall(t, alphaConn, "put_record", map[string]any{"record_id": tc.name, "value": acceptanceNotes})
			base, rev, stop := startGateway(t)
			before := providerCall(t, betaConn, "stats", nil)
			agentCalls := calls.agent.Load()
			totalWrites := int(before["writes_received"].(float64)) + 1

			result := acceptancePublish(t, base, token, tc.name, tc.loseReply)
			if (result["isError"] == true) != tc.loseReply {
				var traces []storage.Trace
				acceptanceAdminGet(t, base+"/api/traces", adminToken, token, &traces)
				t.Fatalf("unexpected MCP result for lose_reply=%t: traces=%v", tc.loseReply, traces)
			}
			if !tc.loseReply && standContent(t, result)["record_id"] != tc.name {
				t.Fatal("successful reply did not identify the written record")
			}

			value := "Summary for " + tc.name + ": " + acceptanceRedacted
			record := providerCall(t, betaConn, "get_record", map[string]any{"record_id": tc.name})
			assertRecord(t, record, "beta", tc.name, value)
			if got := calls.agent.Load() - agentCalls; got != 1 {
				t.Fatalf("agent accepted %d scrubbed calls, want one", got)
			}
			assertBetaWrites := func() {
				stats := providerCall(t, betaConn, "stats", map[string]any{})
				assertStandCount(t, stats, "writes_received", float64(totalWrites))
				assertStandCount(t, stats, "writes_committed", before["writes_committed"].(float64)+1)
			}
			assertBetaWrites()

			effect := acceptanceBetaEffect(t, base, adminToken, token, beta.url, totalWrites)
			if effect.Status != tc.status {
				t.Fatalf("beta effect status=%q, want %q", effect.Status, tc.status)
			}
			assertAcceptanceTraceRevision(t, base, adminToken, token, effect.RunID, rev, "join")
			t.Logf("revision=%s run=%s effect=%s status=%s beta_write_attempts=%d", rev, effect.RunID, effect.ID, effect.Status, totalWrites)
			if !tc.loseReply {
				var history storage.EffectHistory
				acceptanceAdminGet(t, base+"/api/effect-history?effect_id="+effect.ID, adminToken, token, &history)
				assertAcceptanceEffectHistory(t, history, effect, "dispatching", "confirmed")
				return
			}

			stop()
			reopened, reopenedRev, _ := startGateway(t)
			saved := acceptanceBetaEffect(t, reopened, adminToken, token, beta.url, totalWrites)
			if reopenedRev != rev || saved.ID != effect.ID || saved.Status != "unknown" || saved.IntentDigest != effect.IntentDigest {
				t.Fatal("gateway reopen changed the active revision or unresolved effect")
			}
			invalid := jsonPost(t, reopened+"/api/effect-readback", adminToken, map[string]any{
				"effect_id": effect.ID, "adapter": "beta_record",
				"intent": map[string]any{"record_id": tc.name, "value": "mismatched summary", "fault": "drop_after_commit"},
			})
			invalid.Body.Close()
			if invalid.StatusCode != http.StatusBadRequest {
				t.Fatalf("readback accepted a different intent: HTTP %d", invalid.StatusCode)
			}
			if unresolved := acceptanceBetaEffect(t, reopened, adminToken, token, beta.url, totalWrites); unresolved.Status != "unknown" {
				t.Fatal("readback without matching evidence changed the unknown outcome")
			}
			intent := map[string]any{"record_id": tc.name, "value": value, "fault": "drop_after_commit"}
			resolved := decode(t, jsonPost(t, reopened+"/api/effect-readback", adminToken, map[string]any{
				"effect_id": effect.ID, "adapter": "beta_record", "intent": intent,
			}))
			if resolved["status"] != "resolved_applied" {
				t.Fatal("positive readback did not resolve the committed write")
			}
			final := acceptanceBetaEffect(t, reopened, adminToken, token, beta.url, totalWrites)
			if final.ID != effect.ID || final.Status != "resolved_applied" || final.Resolution == nil ||
				final.Resolution.Kind != "readback" || final.Resolution.Verifier != "hcl.readback:beta_record" {
				t.Fatal("resolved effect is missing verified readback provenance")
			}
			var audit []storage.AuditRecord
			acceptanceAdminGet(t, reopened+"/api/audit", adminToken, token, &audit)
			if len(audit) == 0 || audit[0].Action != "regate_effect_readback" || audit[0].Outcome != "succeeded" {
				t.Fatal("successful readback is missing from the management audit")
			}
			history := acceptanceAdminTool(t, reopened, adminToken, "regate_effect_history", map[string]any{"effect_id": effect.ID})
			raw, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			checkAcceptancePrivacy(t, "effect history", raw, token)
			var timeline storage.EffectHistory
			if err := json.Unmarshal(raw, &timeline); err != nil {
				t.Fatal(err)
			}
			assertAcceptanceEffectHistory(t, timeline, effect, "dispatching", "unknown", "resolved_applied")
			v1AuditSnapshot(t, state, source, "checkpoint")
			assertBetaWrites()
			t.Logf("effect=%s status=%s beta_write_attempts=%d", final.ID, final.Status, totalWrites)
		}) {
			return
		}
	}

	t.Run("refusals", func(t *testing.T) {
		base, _, _ := startGateway(t)
		limited := oauth.mint(t, "acceptance:access", "test-user")
		for _, tc := range []struct {
			name, id, notes, bearer string
			alphaReads, scrubs      int
			journalNodes            map[string]string
		}{
			{name: "missing_tool_scope", id: "scope_refused", notes: acceptanceNotes, bearer: limited},
			{name: "decision_denied", id: "DENY-policy", notes: acceptanceNotes, bearer: token,
				journalNodes: map[string]string{"run/node:authorize": "confirmed"}},
			{name: "scrubber_failed", id: "redaction_failed", notes: acceptanceScrubFailure, bearer: token,
				alphaReads: 1, scrubs: 1, journalNodes: map[string]string{"run/node:authorize": "confirmed", "run/node:redact": "unknown"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				providerCall(t, alphaConn, "put_record", map[string]any{"record_id": tc.id, "value": tc.notes})
				alphaReads := providerCall(t, alphaConn, "stats", nil)["reads"].(float64)
				betaStats := providerCall(t, betaConn, "stats", nil)
				scrubs, agents := calls.scrubber.Load(), calls.agent.Load()
				var before []storage.Effect
				acceptanceAdminGet(t, base+"/api/effects", adminToken, token, &before)
				known := make(map[string]bool, len(before))
				for _, effect := range before {
					known[effect.ID] = true
				}

				result := acceptancePublish(t, base, tc.bearer, tc.id, false)
				if result["isError"] != true || result["structuredContent"] != nil {
					t.Fatal("refused invocation returned successful or structured data")
				}
				assertStandCount(t, providerCall(t, alphaConn, "stats", nil), "reads", alphaReads+float64(tc.alphaReads))
				afterBeta := providerCall(t, betaConn, "stats", nil)
				assertStandCount(t, afterBeta, "writes_received", betaStats["writes_received"].(float64))
				assertStandCount(t, afterBeta, "writes_committed", betaStats["writes_committed"].(float64))
				if calls.agent.Load() != agents || calls.scrubber.Load()-scrubs != int64(tc.scrubs) {
					t.Fatal("refused invocation crossed an unexpected hook boundary")
				}

				var after []storage.Effect
				acceptanceAdminGet(t, base+"/api/effects", adminToken, tc.bearer, &after)
				if len(after)-len(before) != len(tc.journalNodes) {
					t.Fatalf("refusal created %d effect entries, want %d", len(after)-len(before), len(tc.journalNodes))
				}
				for _, effect := range after {
					if !known[effect.ID] && tc.journalNodes[effect.Node] != effect.Status {
						t.Fatalf("unexpected effect after refusal: node=%s status=%s", effect.Node, effect.Status)
					}
				}
				var traces []storage.Trace
				acceptanceAdminGet(t, base+"/api/traces", adminToken, tc.bearer, &traces)
				t.Logf("alpha_reads=%d scrubber_calls=%d agent_calls=0 beta_write_attempts=0", tc.alphaReads, tc.scrubs)
			})
		}
	})

	t.Run("recover_oauth_task", func(t *testing.T) {
		const recordID = "approval_task"
		providerCall(t, alphaConn, "put_record", map[string]any{"record_id": recordID, "value": acceptanceNotes})
		alphaReads := providerCall(t, alphaConn, "stats", nil)["reads"].(float64)
		betaWrites := providerCall(t, betaConn, "stats", nil)["writes_received"].(float64)
		base, originalRev, stop := startGateway(t)
		created := acceptanceRPC(t, base+"/mcp/acceptance", token, "tools/call", map[string]any{
			"name":      "publish_summary",
			"arguments": map[string]any{"record_id": recordID, "lose_reply": false, "approval_required": true},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{
					"extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}},
				},
			},
		})
		handle, _ := created["result"].(map[string]any)
		taskID, _ := handle["taskId"].(string)
		if taskID == "" {
			t.Fatalf("async MCP call did not start a task: %v", created["error"])
		}
		waitAcceptanceTask(t, base+"/mcp/acceptance", token, taskID, "input_required")
		assertStandCount(t, providerCall(t, alphaConn, "stats", nil), "reads", alphaReads)
		assertStandCount(t, providerCall(t, betaConn, "stats", nil), "writes_received", betaWrites)

		stop()
		base, reopenedRev, _ := startGateway(t)
		if reopenedRev != originalRev {
			t.Fatal("reopen did not retain the original active revision")
		}
		update := map[string]any{
			"taskId":         taskID,
			"inputResponses": map[string]any{"resume": map[string]any{"action": "accept", "content": true}},
		}
		other := oauth.mint(t, "acceptance:access summary:write", "another-test-user")
		denied := latestCall(t, base+"/mcp/acceptance", other, "tasks/update", 1, update)
		var refusal struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		err := json.NewDecoder(denied.Body).Decode(&refusal)
		denied.Body.Close()
		if err != nil || denied.StatusCode < 400 || refusal.Error.Message != "task not found" {
			t.Fatalf("another OAuth subject could address the task: HTTP %d (%v)", denied.StatusCode, err)
		}
		limited := oauth.mint(t, "acceptance:access", "test-user")
		if got := acceptanceRPC(t, base+"/mcp/acceptance", limited, "tasks/update", update); got["error"] == nil {
			t.Fatal("owner without the original tool scope could resume the task")
		}
		fresh := oauth.mint(t, "acceptance:access summary:write extra:read", "test-user")
		waitAcceptanceTask(t, base+"/mcp/acceptance", fresh, taskID, "input_required")
		assertStandCount(t, providerCall(t, betaConn, "stats", nil), "writes_received", betaWrites)

		activeRev := activateAcceptanceLabel(t, base, adminToken, `"literal:New summary for "`)
		if activeRev == originalRev {
			t.Fatal("activation did not produce a different revision")
		}
		t.Cleanup(func() {
			// A failed assertion still leaves the shared stand on its baseline source.
			acceptanceAdminTool(t, base, adminToken, "regate_rollback", map[string]any{"revision": originalRev})
		})

		updated := acceptanceRPC(t, base+"/mcp/acceptance", fresh, "tasks/update", update)
		if updated["error"] != nil {
			t.Fatalf("owner could not resume task after restart: %v", updated["error"])
		}
		completed := waitAcceptanceTask(t, base+"/mcp/acceptance", fresh, taskID, "completed")
		if result, ok := completed["result"].(map[string]any); !ok || standContent(t, result)["record_id"] != recordID {
			t.Fatal("resumed OAuth task did not return its published record ID")
		}
		record := providerCall(t, betaConn, "get_record", map[string]any{"record_id": recordID})
		assertRecord(t, record, "beta", recordID, "Summary for "+recordID+": "+acceptanceRedacted)
		effect := acceptanceBetaEffect(t, base, adminToken, fresh, beta.url, int(betaWrites)+1)
		assertAcceptanceTraceRevision(t, base, adminToken, fresh, effect.RunID, originalRev, "write")
		assertStandCount(t, providerCall(t, betaConn, "stats", nil), "writes_received", betaWrites+1)
		t.Logf("task=%s revision=%s active_revision=%s effect=%s status=%s", taskID, originalRev, activeRev, effect.ID, effect.Status)
	})

	t.Run("management_mcp", func(t *testing.T) {
		base, _, _ := startGateway(t)
		testAcceptanceAuthoring(t, base, adminToken)
	})

	t.Run("browser", func(t *testing.T) {
		if os.Getenv("REGATE_V1_ACCEPTANCE_BROWSER") != "1" {
			t.Skip("opt in with make test-v1-browser")
		}
		base, _, _ := startGateway(t)
		testAcceptanceBrowser(t, base, adminToken)
	})

	t.Run("live_revision_and_subscription", func(t *testing.T) {
		base, rev, _ := startGateway(t)
		testAcceptanceLiveRevision(t, base, adminToken, token, rev, beta.url, calls, alphaConn, betaConn)
	})

	t.Run("schema_drift", func(t *testing.T) {
		base, rev, _ := startGateway(t)
		testAcceptanceSchemaDrift(t, base, adminToken, token, rev, calls, alphaConn, betaConn)
	})
	if !t.Failed() {
		v1AuditSnapshot(t, state, source, "final")
	}
}

func assertAcceptanceEffectHistory(t *testing.T, history storage.EffectHistory, effect storage.Effect, want ...string) {
	t.Helper()
	if history.EffectID != effect.ID || history.RunID != effect.RunID || history.Node != effect.Node || history.Truncated {
		t.Fatal("effect history cannot be correlated with its pipeline trace")
	}
	statuses := make([]string, 0, len(history.Transitions))
	for _, transition := range history.Transitions {
		if transition.At.IsZero() {
			t.Fatal("effect transition is missing its recorded time")
		}
		statuses = append(statuses, transition.Status)
	}
	if !slices.Equal(statuses, want) {
		t.Fatalf("effect transitions %v, want %v", statuses, want)
	}
}

func acceptanceProviderCall(t *testing.T, conn *mcp.Conn, tool string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := conn.CallTool(ctx, tool, args)
	if err != nil {
		t.Fatal(err)
	}
	return standContent(t, result)
}

func acceptancePublish(t *testing.T, base, token, recordID string, loseReply bool) map[string]any {
	t.Helper()
	reply := acceptanceRPC(t, base+"/mcp/acceptance", token, "tools/call", map[string]any{
		"name":      "publish_summary",
		"arguments": map[string]any{"record_id": recordID, "lose_reply": loseReply},
	})
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatal("missing public MCP tool result")
	}
	return result
}

func acceptanceRPC(t *testing.T, endpoint, token, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := decode(t, latestCall(t, endpoint, token, method, 1, params))
	wire, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	checkAcceptancePrivacy(t, "MCP reply", wire, token)
	return reply
}

func acceptanceAdminTool(t *testing.T, base, token, name string, args map[string]any) map[string]any {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	reply := acceptanceRPC(t, base+"/mcp/admin", token, "tools/call", map[string]any{"name": name, "arguments": args})
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("management MCP %s returned a protocol error: %v", name, reply["error"])
	}
	return standContent(t, result)
}

func waitAcceptanceTask(t *testing.T, endpoint, token, taskID, status string) map[string]any {
	t.Helper()
	var actual any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		reply := acceptanceRPC(t, endpoint, token, "tasks/get", map[string]any{"taskId": taskID})
		result, ok := reply["result"].(map[string]any)
		if !ok {
			t.Fatalf("task lookup returned no result: %v", reply["error"])
		}
		actual = result["status"]
		if actual == status {
			return result
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s stayed at %v, wanted %s", taskID, actual, status)
	return nil
}

func assertAcceptanceTraceRevision(t *testing.T, base, adminToken, token, run, rev, node string) {
	t.Helper()
	var traces []storage.Trace
	acceptanceAdminGet(t, base+"/api/traces?run_id="+run, adminToken, token, &traces)
	found := false
	for _, trace := range traces {
		if trace.RunID != run || trace.Revision != rev {
			t.Fatal("run trace used a different revision")
		}
		found = found || trace.Node == node && trace.Status == "success"
	}
	if !found {
		t.Fatalf("run did not trace successful node %s", node)
	}
}

type acceptanceCalls struct {
	scrubber, agent atomic.Int64
	pause           atomic.Pointer[acceptanceAgentPause]
	setDrift        func(bool)
}

func startAcceptanceSupport(t *testing.T) (*httptest.Server, *rsa.PrivateKey, *acceptanceCalls) {
	t.Helper()
	return startAcceptanceSupportForNotes(t, "Contact ", acceptanceEmail)
}

func startAcceptanceSupportForNotes(t *testing.T, prefix, email string) (*httptest.Server, *rsa.PrivateKey, *acceptanceCalls) {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls acceptanceCalls
	type notes struct {
		Notes string `json:"notes"`
	}
	agent := sdk.NewServer(&sdk.Implementation{Name: "acceptance-summary-agent", Version: "1"}, nil)
	handleNotes := func(ctx context.Context, input notes) error {
		calls.agent.Add(1)
		if input.Notes != prefix+"[REDACTED]" {
			return errors.New("agent expected scrubbed notes")
		}
		if pause := calls.pause.Swap(nil); pause != nil {
			close(pause.entered)
			select {
			case <-pause.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	type revisedOutput struct {
		Overview string `json:"overview"`
	}
	calls.setDrift = func(drifted bool) {
		tool := &sdk.Tool{Name: "summarize"}
		if drifted {
			sdk.AddTool(agent, tool, func(ctx context.Context, _ *sdk.CallToolRequest, input notes) (*sdk.CallToolResult, revisedOutput, error) {
				return nil, revisedOutput{Overview: input.Notes}, handleNotes(ctx, input)
			})
			return
		}
		sdk.AddTool(agent, tool, func(ctx context.Context, _ *sdk.CallToolRequest, input notes) (*sdk.CallToolResult, notes, error) {
			return nil, input, handleNotes(ctx, input)
		})
	}
	calls.setDrift(false)
	router := http.NewServeMux()
	router.Handle("/mcp", sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return agent },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	router.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		key := map[string]string{
			"kid": "test-key", "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.PublicKey.E)).Bytes()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{key}})
	})
	router.HandleFunc("POST /redact", func(w http.ResponseWriter, r *http.Request) {
		calls.scrubber.Add(1)
		var body struct {
			Input notes `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "invalid scrubber input", http.StatusBadRequest)
			return
		}
		if body.Input.Notes == "Fail scrubber for "+email {
			http.Error(w, "synthetic failure for "+email, http.StatusServiceUnavailable)
			return
		}
		if body.Input.Notes != prefix+email {
			http.Error(w, "expected synthetic source notes", http.StatusBadRequest)
			return
		}
		safe := notes{Notes: strings.ReplaceAll(body.Input.Notes, email, "[REDACTED]")}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "replace", "value": safe})
	})
	support := httptest.NewServer(router)
	t.Cleanup(support.Close)
	return support, private, &calls
}

func checkAcceptancePrivacy(t *testing.T, label string, raw []byte, token string) {
	t.Helper()
	if strings.Contains(string(raw), acceptanceEmail) || strings.Contains(string(raw), token) {
		t.Fatalf("%s exposed the synthetic PII marker or bearer", label)
	}
}

func acceptanceAdminGet(t *testing.T, url, adminToken, publicToken string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("management GET %s: HTTP %d", req.URL.Path, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	checkAcceptancePrivacy(t, req.URL.Path, raw, publicToken)
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s: %v", req.URL.Path, err)
	}
}

func acceptanceBetaEffect(t *testing.T, base, adminToken, publicToken, betaURL string, want int) storage.Effect {
	t.Helper()
	var journal []storage.Effect
	acceptanceAdminGet(t, base+"/api/effects", adminToken, publicToken, &journal)
	var latest storage.Effect
	count := 0
	for _, effect := range journal {
		if effect.Operation != "beta.put_record" {
			continue
		}
		if effect.Node != "write" || effect.TargetDigest != storage.TargetDigest("mcp", betaURL) || effect.IntentDigest == "" {
			t.Fatal("beta journal entry is missing a bound target or intent")
		}
		if count == 0 {
			latest = effect
		}
		count++
	}
	if count != want {
		t.Fatalf("beta journal contains %d writes, want %d", count, want)
	}
	return latest
}
