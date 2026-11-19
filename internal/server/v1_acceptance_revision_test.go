package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/storage"
)

type acceptanceAgentPause struct {
	entered, release chan struct{}
}

func testAcceptanceLiveRevision(t *testing.T, base, adminToken, token, rev, betaURL string, calls *acceptanceCalls, alpha, beta *mcp.Conn) {
	t.Helper()
	const oldID, newID = "live_original", "live_revised"
	for _, id := range []string{oldID, newID} {
		acceptanceProviderCall(t, alpha, "put_record", map[string]any{"record_id": id, "value": acceptanceNotes})
	}
	priorWrites := int(acceptanceProviderCall(t, beta, "stats", nil)["writes_received"].(float64))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := base + "/mcp/acceptance"
	subscription := latestRequest(t, url, token, "subscriptions/listen", 17, map[string]any{
		"notifications": map[string]any{"toolsListChanged": true},
	})
	stream, err := http.DefaultClient.Do(subscription.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || !strings.Contains(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("authenticated catalog subscription: HTTP %d", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	if message := acceptanceSSE(t, scanner); message["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatal("catalog subscription was not acknowledged")
	}

	pause := &acceptanceAgentPause{entered: make(chan struct{}), release: make(chan struct{})}
	calls.pause.Store(pause)
	defer calls.pause.CompareAndSwap(pause, nil)
	request := latestRequest(t, url, token, "tools/call", 18, map[string]any{
		"name": "publish_summary", "arguments": map[string]any{"record_id": oldID, "lose_reply": false},
	}).WithContext(ctx)
	completed := make(chan *http.Response)
	failed := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			failed <- err
			return
		}
		select {
		case completed <- response:
		case <-ctx.Done():
			response.Body.Close()
		}
	}()
	select {
	case <-pause.entered:
	case err := <-failed:
		t.Fatalf("live call failed before reaching the agent: %v", err)
	case <-ctx.Done():
		t.Fatal("live call did not reach the agent")
	}

	activated := activateAcceptanceLabel(t, base, adminToken, `"literal:Revised summary for "`)
	if activated == rev {
		t.Fatal("activation did not change the revision")
	}
	defer acceptanceAdminTool(t, base, adminToken, "regate_rollback", map[string]any{"revision": rev})
	event := acceptanceSSE(t, scanner)
	params, _ := event["params"].(map[string]any)
	meta, _ := params["_meta"].(map[string]any)
	if event["method"] != "notifications/tools/list_changed" || meta["io.modelcontextprotocol/subscriptionId"] != float64(17) {
		t.Fatal("activation did not notify the original authenticated subscription")
	}
	assertStandCount(t, acceptanceProviderCall(t, beta, "stats", nil), "writes_received", float64(priorWrites))
	close(pause.release)

	select {
	case response := <-completed:
		body := decode(t, response)
		if result, ok := body["result"].(map[string]any); !ok || standContent(t, result)["record_id"] != oldID {
			t.Fatal("already-started call did not complete")
		}
	case err := <-failed:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("live call did not finish after release")
	}
	first := acceptanceBetaEffect(t, base, adminToken, token, betaURL, priorWrites+1)
	assertAcceptanceTraceRevision(t, base, adminToken, token, first.RunID, rev, "write")
	assertRecord(t, acceptanceProviderCall(t, beta, "get_record", map[string]any{"record_id": oldID}),
		"beta", oldID, "Summary for "+oldID+": "+acceptanceRedacted)

	if result := acceptancePublish(t, base, token, newID, false); standContent(t, result)["record_id"] != newID {
		t.Fatal("new call on activated revision did not succeed")
	}
	second := acceptanceBetaEffect(t, base, adminToken, token, betaURL, priorWrites+2)
	assertAcceptanceTraceRevision(t, base, adminToken, token, second.RunID, activated, "write")
	assertRecord(t, acceptanceProviderCall(t, beta, "get_record", map[string]any{"record_id": newID}),
		"beta", newID, "Revised summary for "+newID+": "+acceptanceRedacted)
	assertStandCount(t, acceptanceProviderCall(t, beta, "stats", nil), "writes_received", float64(priorWrites+2))
	t.Logf("subscription=tools/list_changed old_run=%s old_revision=%s new_run=%s new_revision=%s", first.RunID, rev, second.RunID, activated)
}

func acceptanceSSE(t *testing.T, scanner *bufio.Scanner) map[string]any {
	t.Helper()
	var payload []string
	for scanner.Scan() {
		if line := scanner.Text(); line == "" && len(payload) > 0 {
			var message map[string]any
			if err := json.Unmarshal([]byte(strings.Join(payload, "\n")), &message); err != nil {
				t.Fatalf("decode subscription frame: %v", err)
			}
			return message
		} else if data, ok := strings.CutPrefix(line, "data:"); ok {
			payload = append(payload, strings.TrimSpace(data))
		}
	}
	t.Fatalf("subscription ended before the expected event: %v", scanner.Err())
	return nil
}

func activateAcceptanceLabel(t *testing.T, base, token, expression string) string {
	t.Helper()
	draft := acceptanceAdminTool(t, base, token, "regate_draft_create", nil)
	edit := acceptanceAdminTool(t, base, token, "regate_attribute_patch", map[string]any{
		"draft_id": draft["id"], "expected_source_digest": draft["source_digest"],
		"path": "pipeline/publish_summary/node/label", "attribute": "value", "expression": expression,
	})
	revision := acceptanceAdminTool(t, base, token, "regate_activate", map[string]any{
		"draft_id": edit["id"], "expected_source_digest": edit["source_digest"],
	})
	id, _ := revision["id"].(string)
	if id == "" {
		t.Fatal("activation did not return a revision")
	}
	return id
}

func testAcceptanceSchemaDrift(t *testing.T, base, adminToken, token, rev string, calls *acceptanceCalls, alpha, beta *mcp.Conn) {
	t.Helper()
	const id = "agent_schema_drift"
	acceptanceProviderCall(t, alpha, "put_record", map[string]any{"record_id": id, "value": acceptanceNotes})
	priorWrites := acceptanceProviderCall(t, beta, "stats", nil)["writes_received"].(float64)
	refresh := func(field string) {
		t.Helper()
		catalog := acceptanceAdminTool(t, base, adminToken, "regate_upstream_refresh", map[string]any{"name": "agent"})
		items, ok := catalog["items"].([]any)
		if catalog["revision"] != rev || !ok || len(items) != 1 {
			t.Fatal("agent discovery did not return a revision-bound descriptor")
		}
		tool, _ := items[0].(map[string]any)
		schema, _ := tool["outputSchema"].(map[string]any)
		properties, _ := schema["properties"].(map[string]any)
		if tool["name"] != "summarize" || len(properties) != 1 || properties[field] == nil {
			t.Fatalf("refreshed agent schema did not expose %s", field)
		}
	}
	refresh("notes")
	var before []storage.Effect
	acceptanceAdminGet(t, base+"/api/effects", adminToken, token, &before)
	calls.setDrift(true)
	defer calls.setDrift(false)
	refresh("overview")

	if result := acceptancePublish(t, base, token, id, false); result["isError"] != true || result["structuredContent"] != nil {
		t.Fatal("typed graph accepted the incompatible agent payload")
	}
	var after []storage.Effect
	acceptanceAdminGet(t, base+"/api/effects", adminToken, token, &after)
	var agentEffect storage.Effect
	if len(after)-len(before) != 3 {
		t.Fatal("drifted run did not stop after Decision, redaction and agent")
	}
	for _, effect := range after[:len(after)-len(before)] {
		if effect.Operation == "agent.summarize" && effect.Status == "confirmed" {
			agentEffect = effect
		}
	}
	if agentEffect.ID == "" {
		t.Fatal("agent transport did not succeed before the local contract rejection")
	}
	var traces []storage.Trace
	acceptanceAdminGet(t, base+"/api/traces?run_id="+agentEffect.RunID, adminToken, token, &traces)
	if len(traces) == 0 || traces[0].Status != "failure" || traces[0].Revision != rev {
		t.Fatal("schema rejection did not fail the current run")
	}
	assertStandCount(t, acceptanceProviderCall(t, beta, "stats", nil), "writes_received", priorWrites)
	calls.setDrift(false)
	refresh("notes")
	if result := acceptancePublish(t, base, token, id, false); standContent(t, result)["record_id"] != id {
		t.Fatal("fresh invocation failed after the upstream contract was restored")
	}
	assertStandCount(t, acceptanceProviderCall(t, beta, "stats", nil), "writes_received", priorWrites+1)
	t.Logf("incompatible_schema=overview rejected_run=%s revision=%s rejected_beta_write_attempts=0", agentEffect.RunID, rev)
}
