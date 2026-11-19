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
	token := signedTestJWT(t, private, acceptanceIssuer, acceptanceAudience, "acceptance:access summary:write")

	template, err := os.ReadFile("../../examples/v1-acceptance.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"http://127.0.0.1:9201", strings.TrimSuffix(alpha.url, "/mcp"),
		"http://127.0.0.1:9202", strings.TrimSuffix(beta.url, "/mcp"),
		"http://127.0.0.1:9203", support.URL,
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
		return api.URL, rev.ID, closeGateway
	}

	alphaConn := mcp.NewConn(alpha.url, "", 5*time.Second)
	betaConn := mcp.NewConn(beta.url, "", 5*time.Second)
	providerCall := func(t *testing.T, conn *mcp.Conn, tool string, args map[string]any) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := conn.CallTool(ctx, tool, args)
		if err != nil {
			t.Fatal(err)
		}
		return standContent(t, result)
	}

	for i, tc := range []struct {
		name, status string
		loseReply    bool
	}{
		{name: "confirmed", status: "confirmed"},
		{name: "lost_reply", status: "unknown", loseReply: true},
	} {
		if !t.Run(tc.name, func(t *testing.T) {
			providerCall(t, alphaConn, "put_record", map[string]any{"record_id": tc.name, "value": acceptanceNotes})
			base, rev, stop := startGateway(t)

			result := acceptancePublish(t, base, token, tc.name, tc.loseReply)
			if (result["isError"] == true) != tc.loseReply {
				t.Fatalf("unexpected MCP result for lose_reply=%t", tc.loseReply)
			}
			if !tc.loseReply && standContent(t, result)["record_id"] != tc.name {
				t.Fatal("successful reply did not identify the written record")
			}

			value := "Summary for " + tc.name + ": " + acceptanceRedacted
			record := providerCall(t, betaConn, "get_record", map[string]any{"record_id": tc.name})
			assertRecord(t, record, "beta", tc.name, value)
			if got := calls.agent.Load(); got != int64(i+1) {
				t.Fatalf("agent accepted %d scrubbed calls, want %d", got, i+1)
			}
			assertBetaWrites := func() {
				stats := providerCall(t, betaConn, "stats", map[string]any{})
				assertStandCount(t, stats, "writes_received", float64(i+1))
				assertStandCount(t, stats, "writes_committed", float64(i+1))
			}
			assertBetaWrites()

			effect := acceptanceBetaEffect(t, base, adminToken, token, beta.url, i+1)
			if effect.Status != tc.status {
				t.Fatalf("beta effect status=%q, want %q", effect.Status, tc.status)
			}
			var traces []storage.Trace
			acceptanceAdminGet(t, base+"/api/traces?run_id="+effect.RunID, adminToken, token, &traces)
			found := false
			for _, trace := range traces {
				if trace.RunID != effect.RunID || trace.Revision != rev {
					t.Fatal("run trace was not bound to the active revision")
				}
				found = found || trace.Node == "join" && trace.Status == "success"
			}
			if !found {
				t.Fatal("the typed join did not leave a successful trace")
			}
			t.Logf("revision=%s run=%s effect=%s status=%s beta_write_attempts=%d", rev, effect.RunID, effect.ID, effect.Status, i+1)
			if !tc.loseReply {
				return
			}

			stop()
			reopened, reopenedRev, _ := startGateway(t)
			saved := acceptanceBetaEffect(t, reopened, adminToken, token, beta.url, i+1)
			if reopenedRev != rev || saved.ID != effect.ID || saved.Status != "unknown" || saved.IntentDigest != effect.IntentDigest {
				t.Fatal("gateway reopen changed the active revision or unresolved effect")
			}
			intent := map[string]any{"record_id": tc.name, "value": value, "fault": "drop_after_commit"}
			resolved := decode(t, jsonPost(t, reopened+"/api/effect-readback", adminToken, map[string]any{
				"effect_id": effect.ID, "adapter": "beta_record", "intent": intent,
			}))
			if resolved["status"] != "resolved_applied" {
				t.Fatal("positive readback did not resolve the committed write")
			}
			final := acceptanceBetaEffect(t, reopened, adminToken, token, beta.url, i+1)
			if final.ID != effect.ID || final.Status != "resolved_applied" || final.Resolution == nil ||
				final.Resolution.Kind != "readback" || final.Resolution.Verifier != "hcl.readback:beta_record" {
				t.Fatal("resolved effect is missing verified readback provenance")
			}
			var audit []storage.AuditRecord
			acceptanceAdminGet(t, reopened+"/api/audit", adminToken, token, &audit)
			if len(audit) == 0 || audit[0].Action != "regate_effect_readback" || audit[0].Outcome != "succeeded" {
				t.Fatal("successful readback is missing from the management audit")
			}
			assertBetaWrites()
			t.Logf("effect=%s status=%s beta_write_attempts=%d", final.ID, final.Status, i+1)
		}) {
			return
		}
	}

	t.Run("refusals", func(t *testing.T) {
		base, _, _ := startGateway(t)
		limited := signedTestJWT(t, private, acceptanceIssuer, acceptanceAudience, "acceptance:access")
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
}

func acceptancePublish(t *testing.T, base, token, recordID string, loseReply bool) map[string]any {
	t.Helper()
	reply := decode(t, latestCall(t, base+"/mcp/acceptance", token, "tools/call", 1, map[string]any{
		"name":      "publish_summary",
		"arguments": map[string]any{"record_id": recordID, "lose_reply": loseReply},
	}))
	wire, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	checkAcceptancePrivacy(t, "public MCP reply", wire, token)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatal("missing public MCP tool result")
	}
	return result
}

type acceptanceCalls struct {
	scrubber, agent atomic.Int64
}

func startAcceptanceSupport(t *testing.T) (*httptest.Server, *rsa.PrivateKey, *acceptanceCalls) {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var calls acceptanceCalls
	type notes struct {
		Notes string `json:"notes"`
	}
	type summary struct {
		Summary string `json:"summary"`
	}
	agent := sdk.NewServer(&sdk.Implementation{Name: "acceptance-summary-agent", Version: "1"}, nil)
	sdk.AddTool(agent, &sdk.Tool{Name: "summarize"}, func(_ context.Context, _ *sdk.CallToolRequest, input notes) (*sdk.CallToolResult, summary, error) {
		calls.agent.Add(1)
		if input.Notes != acceptanceRedacted {
			return nil, summary{}, errors.New("agent expected scrubbed notes")
		}
		return nil, summary{Summary: input.Notes}, nil
	})
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
		if body.Input.Notes == acceptanceScrubFailure {
			http.Error(w, "synthetic failure for "+acceptanceEmail, http.StatusServiceUnavailable)
			return
		}
		if body.Input.Notes != acceptanceNotes {
			http.Error(w, "expected synthetic source notes", http.StatusBadRequest)
			return
		}
		safe := notes{Notes: strings.ReplaceAll(body.Input.Notes, acceptanceEmail, "[REDACTED]")}
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
