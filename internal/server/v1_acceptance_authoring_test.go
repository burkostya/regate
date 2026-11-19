package server

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func testAcceptanceAuthoring(t *testing.T, base, adminToken string) {
	t.Helper()
	admin := func(name string, args map[string]any) map[string]any {
		t.Helper()
		return acceptanceAdminTool(t, base, adminToken, name, args)
	}
	endpoint := base + "/mcp/admin"
	const skillURI = "skill://regate-authoring/SKILL.md"
	skills := acceptanceRPC(t, endpoint, adminToken, "skills/list", nil)
	skillsJSON, err := json.Marshal(skills)
	if err != nil || !strings.Contains(string(skillsJSON), skillURI) {
		t.Fatal("authoring skill absent from management MCP discovery")
	}
	resource := acceptanceRPC(t, endpoint, adminToken, "resources/read", map[string]any{"uri": skillURI})
	resourceJSON, err := json.Marshal(resource)
	if err != nil || !strings.Contains(string(resourceJSON), "Authoring workflow") {
		t.Fatal("could not read the published authoring skill")
	}

	baseline := admin("regate_config_get", nil)
	source, ok := baseline["source"].(string)
	if !ok || baseline["revision"] == "" {
		t.Fatal("management MCP did not return the active source")
	}
	if graph := admin("regate_graph_get", map[string]any{"pipeline": "publish_summary"}); graph["entry"] != "authorize" {
		t.Fatal("management MCP did not expose the scenario graph")
	}
	admin("regate_upstream_refresh", map[string]any{"name": "alpha"})
	admin("regate_upstream_refresh", map[string]any{"name": "beta"})
	draft := admin("regate_draft_create", nil)
	save := func(draft map[string]any, source string) map[string]any {
		t.Helper()
		return admin("regate_draft_patch", map[string]any{
			"draft_id": draft["id"], "expected_source_digest": draft["source_digest"], "source": source,
		})
	}
	const opaque = "\n    vendor_extension = acceptance.extra"
	const label = `node "label" {`
	opaqueSource := strings.Replace(source, label, label+opaque, 1)
	if opaqueSource == source {
		t.Fatal("baseline had no label node to edit")
	}
	draft = save(draft, opaqueSource)
	patch := admin("regate_attribute_patch", map[string]any{
		"draft_id": draft["id"], "expected_source_digest": draft["source_digest"],
		"path": "pipeline/publish_summary/node/label", "attribute": "value",
		"expression": `"literal:Reviewed summary for "`,
	})
	edited, ok := patch["source"].(string)
	if !ok || !strings.Contains(edited, opaque) || !strings.Contains(edited, "literal:Reviewed summary for ") {
		t.Fatal("patch failed to preserve the unrelated opaque attribute")
	}
	if validation := admin("regate_validate", map[string]any{"draft_id": patch["id"]}); validation["valid"] != false {
		t.Fatal("unrecognized draft HCL passed executable validation")
	}
	reject := acceptanceRPC(t, endpoint, adminToken, "tools/call", map[string]any{
		"name": "regate_activate", "arguments": map[string]any{
			"draft_id": patch["id"], "expected_source_digest": patch["source_digest"],
		},
	})
	if result, ok := reject["result"].(map[string]any); !ok || result["isError"] != true {
		t.Fatal("management MCP accepted activation with opaque HCL")
	}
	if active := admin("regate_config_get", nil); active["revision"] != baseline["revision"] {
		t.Fatal("failed activation changed the active revision")
	}

	draft = save(patch, strings.Replace(edited, opaque, "", 1))
	if validation := admin("regate_validate", map[string]any{"draft_id": draft["id"]}); validation["valid"] != true {
		t.Fatalf("reviewed draft failed validation: %v", validation["error"])
	}
	dry := admin("regate_test", map[string]any{
		"draft_id": draft["id"], "expected_source_digest": draft["source_digest"],
		"surface": "acceptance", "tool": "publish_summary",
		"arguments": map[string]any{"record_id": "preview", "lose_reply": false},
	})
	if value, ok := dry["value"].(map[string]any); dry["status"] != "success" || !ok || value["record_id"] != "preview" {
		t.Fatal("draft dry-run did not return the mock record")
	}
	diff := admin("regate_diff", map[string]any{"draft_id": draft["id"]})
	changes, ok := diff["semantic_changes"].([]any)
	if !ok || len(changes) != 1 {
		t.Fatal("semantic diff did not isolate the label edit")
	}
	if edit, ok := changes[0].(map[string]any); !ok || edit["path"] != "pipelines.publish_summary.nodes.label.attrs.value" ||
		edit["after"] != "literal:Reviewed summary for " || diff["conflict"] != false {
		t.Fatal("semantic diff did not identify the reviewed value")
	}
	admin("regate_activate", map[string]any{"draft_id": draft["id"], "expected_source_digest": draft["source_digest"]})
	active := admin("regate_config_get", nil)
	if active["revision"] == baseline["revision"] || active["source"] != draft["source"] {
		t.Fatal("active configuration does not match the reviewed draft")
	}
	admin("regate_rollback", map[string]any{"revision": baseline["revision"]})
	restored := admin("regate_config_get", nil)
	if restored["revision"] == active["revision"] || restored["revision"] == baseline["revision"] || restored["source"] != source {
		t.Fatal("rollback did not publish the original source as a new revision")
	}
	t.Logf("management_mcp base=%s activated=%s restored=%s", baseline["revision"], active["revision"], restored["revision"])
}

func testAcceptanceBrowser(t *testing.T, base, adminToken string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	web := exec.CommandContext(ctx, "node", "build")
	web.Dir = "../../web"
	web.Env = append(os.Environ(), "HOST=127.0.0.1", "PORT=0", "SOCKET_PATH=", "REGATE_PROXY="+base)
	web.Stderr = os.Stderr
	stdout, err := web.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := web.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = web.Process.Kill()
		_ = web.Wait()
	}()
	ready := make(chan string, 1)
	go func() {
		defer close(ready)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if endpoint, ok := strings.CutPrefix(scanner.Text(), "Listening on http://127.0.0.1:"); ok {
				ready <- "http://127.0.0.1:" + endpoint
				return
			}
		}
	}()
	var browserURL string
	select {
	case browserURL = <-ready:
		if browserURL == "" {
			t.Fatal("production console stopped before binding its loopback listener; run npm run build in web")
		}
	case <-ctx.Done():
		t.Fatal("production console did not start")
	}
	cmd := exec.CommandContext(ctx, "npm", "run", "test:e2e", "--", "v1-acceptance.spec.ts")
	cmd.Dir = "../../web"
	cmd.Env = append(os.Environ(),
		"REGATE_E2E_COMPOSED_URL="+browserURL,
		"REGATE_E2E_ADMIN_TOKEN="+adminToken,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Chromium acceptance: %v\n%s", err, output)
	}
	t.Logf("Chromium acceptance:\n%s", output)
}
