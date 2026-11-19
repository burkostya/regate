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
	acceptanceIssuer   = "https://issuer.test"
	acceptanceAudience = "https://regate.test/mcp/acceptance"
	acceptanceEmail    = "acceptance-marker@example.com"
	acceptanceNotes    = "Contact " + acceptanceEmail
	acceptanceRedacted = "Contact [REDACTED]"
)

// Both writes go through the same OAuth-protected graph. Alpha and beta are
// separate SDK processes, and only the gateway is reopened for readback.
func TestV1Acceptance(t *testing.T) {
	const adminToken = "v1-acceptance-local-admin"
	t.Setenv("REGATE_ADMIN_TOKEN", adminToken)
	t.Setenv("REGATE_OTLP_TRACES_URL", "")

	root := t.TempDir()
	alpha := startStandProcess(t, "alpha", filepath.Join(root, "alpha"))
	beta := startStandProcess(t, "beta", filepath.Join(root, "beta"))
	support, private, agentCalls := startAcceptanceSupport(t)
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

			reply := decode(t, latestCall(t, base+"/mcp/acceptance", token, "tools/call", 1, map[string]any{
				"name":      "publish_summary",
				"arguments": map[string]any{"record_id": tc.name, "lose_reply": tc.loseReply},
			}))
			wire, err := json.Marshal(reply)
			if err != nil {
				t.Fatal(err)
			}
			checkAcceptancePrivacy(t, "public MCP reply", wire, token)
			result, ok := reply["result"].(map[string]any)
			if !ok || (result["isError"] == true) != tc.loseReply {
				t.Fatalf("unexpected MCP result for lose_reply=%t", tc.loseReply)
			}
			if !tc.loseReply && standContent(t, result)["record_id"] != tc.name {
				t.Fatal("successful reply did not identify the written record")
			}

			value := "Summary for " + tc.name + ": " + acceptanceRedacted
			record := providerCall(t, betaConn, "get_record", map[string]any{"record_id": tc.name})
			assertRecord(t, record, "beta", tc.name, value)
			if got := agentCalls.Load(); got != int64(i+1) {
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
}

func startAcceptanceSupport(t *testing.T) (*httptest.Server, *rsa.PrivateKey, *atomic.Int64) {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var agentCalls atomic.Int64
	type notes struct {
		Notes string `json:"notes"`
	}
	type summary struct {
		Summary string `json:"summary"`
	}
	agent := sdk.NewServer(&sdk.Implementation{Name: "acceptance-summary-agent", Version: "1"}, nil)
	sdk.AddTool(agent, &sdk.Tool{Name: "summarize"}, func(_ context.Context, _ *sdk.CallToolRequest, input notes) (*sdk.CallToolResult, summary, error) {
		if input.Notes != acceptanceRedacted {
			return nil, summary{}, errors.New("agent expected scrubbed notes")
		}
		agentCalls.Add(1)
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
		var body struct {
			Input notes `json:"input"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Input.Notes != acceptanceNotes {
			http.Error(w, "expected synthetic source notes", http.StatusBadRequest)
			return
		}
		safe := notes{Notes: strings.ReplaceAll(body.Input.Notes, acceptanceEmail, "[REDACTED]")}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"action": "replace", "value": safe})
	})
	support := httptest.NewServer(router)
	t.Cleanup(support.Close)
	return support, private, &agentCalls
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
