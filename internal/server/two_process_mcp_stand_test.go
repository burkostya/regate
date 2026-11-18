package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/storage"
	"github.com/burkostya/regate/internal/testmcp"
)

// This helper is executed as a separate OS process by the acceptance test.
// Each process binds an independent loopback socket and owns a distinct
// fsynced state file. The actual CLI uses the same testmcp implementation.
func TestMCPStandHelperProcess(t *testing.T) {
	instance := os.Getenv("REGATE_MCP_STAND_HELPER")
	if instance == "" {
		return
	}
	s, err := testmcp.New(instance, os.Getenv("REGATE_MCP_STAND_STATE"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("READY http://%s/mcp\n", listener.Addr())
	if err := s.Serve(context.Background(), listener); err != nil {
		t.Fatal(err)
	}
}

type standProcess struct {
	cmd  *exec.Cmd
	url  string
	dir  string
	once sync.Once
}

func (p *standProcess) stop() {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
}

func startStandProcess(t *testing.T, instance, dir string) *standProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPStandHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), "REGATE_MCP_STAND_HELPER="+instance,
		"REGATE_MCP_STAND_STATE="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// stderr is diagnostic only. No credentials or arbitrary remote inputs
	// are ever passed to this fixture.
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := &standProcess{cmd: cmd, dir: dir}
	t.Cleanup(proc.stop)
	ready := make(chan string, 1)
	failed := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "READY ") {
				ready <- strings.TrimPrefix(line, "READY ")
				return
			}
		}
		failed <- fmt.Errorf("fixture %s failed before READY: %v", instance, scanner.Err())
	}()
	select {
	case proc.url = <-ready:
	case err := <-failed:
		proc.stop()
		t.Fatal(err)
	case <-time.After(8 * time.Second):
		proc.stop()
		t.Fatalf("fixture %s did not announce readiness", instance)
	}
	if !strings.HasPrefix(proc.url, "http://127.0.0.1:") ||
		!strings.HasSuffix(proc.url, "/mcp") {
		proc.stop()
		t.Fatalf("fixture reported unsafe URL: %s", proc.url)
	}
	health := strings.TrimSuffix(proc.url, "/mcp") + "/healthz"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for i := 0; i < 40; i++ {
		response, err := client.Get(health)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return proc
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	proc.stop()
	t.Fatalf("fixture %s never became healthy", instance)
	return nil
}

func standCall(t *testing.T, base, surface, tool string, args map[string]any) map[string]any {
	t.Helper()
	response := latestCall(t, base+"/mcp/"+surface, "", "tools/call", 95, map[string]any{
		"name": tool, "arguments": args,
	})
	body := decode(t, response)
	if body["error"] != nil {
		t.Fatalf("MCP %s.%s protocol error: %#v", surface, tool, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("MCP %s.%s response missing result: %#v", surface, tool, body)
	}
	return result
}

func standContent(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	if result["isError"] == true {
		t.Fatalf("MCP fixture returned an application error: %v", result)
	}
	data, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("MCP fixture did not return structuredContent: %v", result)
	}
	return data
}

func standStats(t *testing.T, base, instance string) map[string]any {
	t.Helper()
	return standContent(t, standCall(t, base, instance, instance+"_stats", map[string]any{}))
}

func assertStandCount(t *testing.T, stats map[string]any, field string, want float64) {
	t.Helper()
	if stats[field] != want {
		t.Fatalf("fixture %v: %s=%v, want %v", stats["instance"], field, stats[field], want)
	}
}

func assertRecord(t *testing.T, content map[string]any, instance, id, value string) {
	t.Helper()
	record, ok := content["record"].(map[string]any)
	if !ok || content["found"] != true ||
		record["instance"] != instance || record["record_id"] != id || record["value"] != value {
		t.Fatalf("unexpected %s record %s: %#v", instance, id, content)
	}
}

func listStandCatalog(t *testing.T, base, surface, method, key string) []map[string]any {
	t.Helper()
	response := latestCall(t, base+"/mcp/"+surface, "", method, 98, map[string]any{})
	body := decode(t, response)
	if body["error"] != nil {
		t.Fatalf("%s %s: %v", surface, method, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("catalog result malformed: %#v", body)
	}
	list, ok := result[key].([]any)
	if !ok {
		t.Fatalf("catalog missing %s: %#v", key, result)
	}
	output := make([]map[string]any, 0, len(list))
	for _, element := range list {
		entry, ok := element.(map[string]any)
		if !ok {
			t.Fatalf("malformed catalog entry: %v", element)
		}
		output = append(output, entry)
	}
	return output
}

// TestTwoProcessMCPStand is a deterministic integration contract, NOT a
// third-party provider certification. These are real independent OS
// processes using the official MCP SDK and separate persistent state files.
//
// Covered: isolation and catalog curation, read/write, fail-closed Decision,
// typed DAG composition, post-commit lost response, unknown journal, verified
// positive readback, mismatched intent/endpoint rejection, no auto retry,
// and provider restart with the original persisted record still present.
func TestTwoProcessMCPStand(t *testing.T) {
	t.Setenv("REGATE_ADMIN_TOKEN", "two-mcp-stand-admin")
	state := t.TempDir()
	alphaDir := filepath.Join(state, "alpha")
	betaDir := filepath.Join(state, "beta")
	alpha := startStandProcess(t, "alpha", alphaDir)
	beta := startStandProcess(t, "beta", betaDir)
	if alpha.cmd.Process.Pid == beta.cmd.Process.Pid || alpha.url == beta.url {
		t.Fatal("MCP stand did not start two independently isolated OS processes")
	}

	b, err := os.ReadFile("../../examples/two-mcp-stand.hcl")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(b), "http://127.0.0.1:9201", strings.TrimSuffix(alpha.url, "/mcp"))
	src = strings.ReplaceAll(src, "http://127.0.0.1:9202", strings.TrimSuffix(beta.url, "/mcp"))
	if strings.Contains(src, "http://127.0.0.1:9201") ||
		strings.Contains(src, "http://127.0.0.1:9202") {
		t.Fatal("stand did not substitute every provider endpoint")
	}

	svc, err := New(filepath.Join(state, "gateway"), src)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Store.Close()
	api := httptest.NewServer(svc.Handler())
	defer api.Close()
	base := api.URL

	for _, instance := range []string{"alpha", "beta"} {
		other := "alpha"
		if instance == "alpha" {
			other = "beta"
		}
		tools := listStandCatalog(t, base, instance, "tools/list", "tools")
		names := map[string]bool{}
		for _, entry := range tools {
			names[entry["name"].(string)] = true
		}
		if len(names) != 3 || !names[instance+"_put"] || !names[instance+"_get"] ||
			!names[instance+"_stats"] || names[other+"_put"] {
			t.Fatalf("%s catalog leaked tools: %#v", instance, names)
		}
		resources := listStandCatalog(t, base, instance, "resources/list", "resources")
		prompts := listStandCatalog(t, base, instance, "prompts/list", "prompts")
		if len(resources) != 1 || resources[0]["uri"] != "fixture://"+instance+"/guide" ||
			len(prompts) != 1 || prompts[0]["name"] != "review" {
			t.Fatalf("%s resource/prompt curation failed: %#v %#v", instance, resources, prompts)
		}
	}
	if tools := listStandCatalog(t, base, "workflow", "tools/list", "tools"); len(tools) != 1 ||
		tools[0]["name"] != "copy" {
		t.Fatalf("workflow surface exposes extra tools: %#v", tools)
	}

	alphaRecord := map[string]any{"record_id": "seed_alpha", "value": "synthetic_alpha"}
	betaRecord := map[string]any{"record_id": "seed_beta", "value": "synthetic_beta"}
	assertRecord(t, standContent(t, standCall(t, base, "alpha", "alpha_put", alphaRecord)),
		"alpha", "seed_alpha", "synthetic_alpha")
	assertRecord(t, standContent(t, standCall(t, base, "beta", "beta_put", betaRecord)),
		"beta", "seed_beta", "synthetic_beta")

	assertRecord(t, standContent(t, standCall(t, base, "alpha", "alpha_get",
		map[string]any{"record_id": "seed_alpha"})), "alpha", "seed_alpha", "synthetic_alpha")
	if content := standContent(t, standCall(t, base, "beta", "beta_get",
		map[string]any{"record_id": "seed_alpha"})); content["found"] != false {
		t.Fatalf("beta read alpha's private state: %v", content)
	}
	assertRecord(t, standContent(t, standCall(t, base, "workflow", "copy",
		map[string]any{"record_id": "seed_alpha"})), "beta", "seed_alpha", "synthetic_alpha")
	if fail := standCall(t, base, "workflow", "copy",
		map[string]any{"record_id": "DENY_secret"}); fail["isError"] != true {
		t.Fatalf("Decision denial did not fail closed: %#v", fail)
	}
	if fail := standCall(t, base, "workflow", "copy",
		map[string]any{"record_id": "not_found"}); fail["isError"] != true {
		t.Fatalf("missing source record triggered a successful beta write: %#v", fail)
	}
	assertStandCount(t, standStats(t, base, "alpha"), "writes_received", 1)
	assertStandCount(t, standStats(t, base, "beta"), "writes_received", 2)

	// The fixture commits the record + fsyncs its state before dropping
	// the TCP response. The Regate effect journal must remain UNKNOWN.
	lostIntent := map[string]any{
		"record_id": "lost_response", "value": "durable_synthetic_data",
		"fault": "drop_after_commit",
	}
	if result := standCall(t, base, "alpha", "alpha_put", lostIntent); result["isError"] != true {
		t.Fatalf("lost post-commit response was reported successful: %#v", result)
	}
	var unknown storage.Effect
	var seen int
	for _, effect := range svc.Store.Effects() {
		if effect.Operation == "alpha.put_record" && effect.Status == "unknown" {
			unknown = effect
			seen++
		}
	}
	if seen != 1 || unknown.IntentDigest == "" ||
		unknown.TargetDigest != storage.TargetDigest("mcp", alpha.url) {
		t.Fatalf("lost write not bound as a unique UNKNOWN effect: %+v (%d)", unknown, seen)
	}
	assertRecord(t, standContent(t, standCall(t, base, "alpha", "alpha_get",
		map[string]any{"record_id": "lost_response"})), "alpha", "lost_response", "durable_synthetic_data")
	assertStandCount(t, standStats(t, base, "alpha"), "writes_received", 2)
	assertStandCount(t, standStats(t, base, "alpha"), "writes_committed", 2)

	betaReads := standStats(t, base, "beta")["reads"]
	badAdapter := jsonPost(t, base+"/api/effect-readback", "two-mcp-stand-admin",
		map[string]any{"effect_id": unknown.ID, "adapter": "beta_record", "intent": lostIntent})
	_ = badAdapter.Body.Close()
	if badAdapter.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong provider reconciled alpha's write: HTTP %d", badAdapter.StatusCode)
	}
	if got := standStats(t, base, "beta")["reads"]; got != betaReads {
		t.Fatalf("wrong-provider readback was dispatched: %v -> %v", betaReads, got)
	}
	badIntent := map[string]any{
		"record_id": "lost_response", "value": "forged_value", "fault": "drop_after_commit",
	}
	bad := jsonPost(t, base+"/api/effect-readback", "two-mcp-stand-admin",
		map[string]any{"effect_id": unknown.ID, "adapter": "alpha_record", "intent": badIntent})
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("changed original intent was accepted: HTTP %d", bad.StatusCode)
	}
	saved, err := svc.Store.EffectGet(unknown.ID)
	if err != nil || saved.Status != "unknown" {
		t.Fatalf("invalid evidence changed journal state: %+v %v", saved, err)
	}

	resolved := decode(t, jsonPost(t, base+"/api/effect-readback", "two-mcp-stand-admin",
		map[string]any{"effect_id": unknown.ID, "adapter": "alpha_record", "intent": lostIntent}))
	if resolved["status"] != "resolved_applied" {
		t.Fatalf("positive provider readback did not resolve unknown effect: %#v", resolved)
	}
	evidence, ok := resolved["resolution"].(map[string]any)
	if !ok || evidence["kind"] != "readback" ||
		evidence["verifier"] != "hcl.readback:alpha_record" || evidence["verdict"] != "applied" {
		t.Fatalf("journal lost readback provenance: %#v", resolved)
	}
	again := jsonPost(t, base+"/api/effect-readback", "two-mcp-stand-admin",
		map[string]any{"effect_id": unknown.ID, "adapter": "alpha_record", "intent": lostIntent})
	_ = again.Body.Close()
	if again.StatusCode != http.StatusBadRequest {
		t.Fatalf("second reconciliation was accepted: HTTP %d", again.StatusCode)
	}
	if received := standStats(t, base, "alpha")["writes_received"]; received != float64(2) {
		t.Fatalf("readback caused a duplicate write: %v", received)
	}

	// Abruptly terminate alpha (not the gateway or beta) and launch another
	// OS process from the SAME dedicated directory. Query the official MCP
	// SDK directly; no Regate URL rewrite or fresh write is permitted.
	oldPID := alpha.cmd.Process.Pid
	alpha.stop()
	restarted := startStandProcess(t, "alpha", alphaDir)
	if restarted.cmd.Process.Pid == oldPID || restarted.url == beta.url {
		t.Fatalf("provider restart did not isolate the new worker: old=%d new=%d", oldPID, restarted.cmd.Process.Pid)
	}
	conn := mcp.NewConn(restarted.url, "", 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	raw, err := conn.CallTool(ctx, "get_record", map[string]any{"record_id": "lost_response"})
	if err != nil {
		t.Fatalf("read after provider restart: %v", err)
	}
	assertRecord(t, standContent(t, raw), "alpha", "lost_response", "durable_synthetic_data")
	stats, err := conn.CallTool(ctx, "stats", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	actualStats, ok := stats["structuredContent"].(map[string]any)
	if !ok || actualStats["writes_received"] != float64(2) ||
		actualStats["writes_committed"] != float64(2) {
		t.Fatalf("provider restart lost commit or attempt history: %#v", stats)
	}
	assertStandCount(t, standStats(t, base, "beta"), "writes_received", 2)
}
