package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func resumedResponseFixture() string {
	return `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "pause" {
    kind = "suspend"
  }
  node "done" {
    kind = "output"
    value = "nodes.pause.value"
  }
  edge {
    from = "start"
    to = "pause"
  }
  edge {
    from = "pause"
    to = "done"
  }
}
surface "root" {
  path = "/mcp/root"
  tool "read" {
    pipeline = "root"
    read_only = true
    output_schema = {type = "string"}
  }
}
`
}

func withBuiltinResponseMask(source string) string {
	return `hook "response_mask" {
  transport = "builtin"
  contract = "transform"
}
interceptor "response_filter" {
  stage = "before_response"
  hook = "response_mask"
}
` + source
}

func suspendedResponseRun(t *testing.T, source string) (*Runtime, *config.Config, string) {
	t.Helper()
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	rt := New(effectTestStore(t))
	out, err := rt.Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, false)
	if err == nil || out.Status != "suspended" {
		t.Fatalf("expected response suspension: %#v %v", out, err)
	}
	return rt, cfg, out.RunID
}

func TestResumeAppliesResponseMaskBeforePublishing(t *testing.T) {
	rt, cfg, id := suspendedResponseRun(t, withBuiltinResponseMask(resumedResponseFixture()))
	out, err := rt.Resume(context.Background(), cfg, "rev", id, "alice@example.com")
	if err != nil || out.Status != "success" || out.Value != "[EMAIL_REDACTED]" {
		t.Fatalf("resumed response missed response hook: %#v %v", out, err)
	}
	var cp Checkpoint
	if err := rt.Store.GetSuspended(id, &cp); err != sql.ErrNoRows {
		t.Fatalf("completed checkpoint not consumed: %v", err)
	}
}

func TestResumeRejectsToolOutputSchemaAndConsumesCheckpoint(t *testing.T) {
	rt, cfg, id := suspendedResponseRun(t, resumedResponseFixture())
	out, err := rt.Resume(context.Background(), cfg, "rev", id, 42)
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("invalid resumed tool result accepted: %#v %v", out, err)
	}
	if out.Status != "failure" || out.Value != nil || out.OutputProduced || out.MCP != nil {
		t.Fatalf("invalid result escaped published boundary: %#v", out)
	}
	var cp Checkpoint
	if err := rt.Store.GetSuspended(id, &cp); err != sql.ErrNoRows {
		t.Fatalf("invalid completed checkpoint is still resumable: %v", err)
	}
	if _, err = rt.Resume(context.Background(), cfg, "rev", id, "valid"); err == nil {
		t.Fatal("second resume succeeded after invalid finished output")
	}
}

func TestResumeRevalidatesAfterResponseTransformer(t *testing.T) {
	// An email satisfies the original tool schema, but the mandatory
	// redaction removes '@', so post-transform validation must reject it.
	src := strings.Replace(resumedResponseFixture(),
		`output_schema = {type = "string"}`,
		`output_schema = {type = "string", pattern = "@"}`, 1)
	rt, cfg, id := suspendedResponseRun(t, withBuiltinResponseMask(src))
	out, err := rt.Resume(context.Background(), cfg, "rev", id, "alice@example.com")
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("invalid transformed result accepted: %#v %v", out, err)
	}
	if out.Value != nil || out.OutputProduced || out.Status != "failure" {
		t.Fatalf("transformed result leaked: %#v", out)
	}
}

func TestResponseHookRunsOnceOnlyAfterResumption(t *testing.T) {
	var calls atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			t.Errorf("bad hook input: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"action":"replace", "value":"approved"})
	}))
	defer hook.Close()
	src := fmt.Sprintf(`hook "response_rewrite" {
  transport = "http"
  url = %q
  contract = "transform"
  timeout_ms = 5000
}
interceptor "response_filter" {
  stage = "before_response"
  hook = "response_rewrite"
}
`, hook.URL) + resumedResponseFixture()
	rt, cfg, id := suspendedResponseRun(t, src)
	if calls.Load() != 0 {
		t.Fatalf("before_response fired before suspension settled: %d", calls.Load())
	}
	out, err := rt.Resume(context.Background(), cfg, "rev", id, "pending")
	if err != nil || out.Value != "approved" || calls.Load() != 1 {
		t.Fatalf("resumed hook not applied exactly once: %#v %v calls=%d", out, err, calls.Load())
	}
	if _, err = rt.Resume(context.Background(), cfg, "rev", id, "again"); err == nil || calls.Load() != 1 {
		t.Fatalf("response hook replayed after checkpoint consumption: %v calls=%d", err, calls.Load())
	}
}

func TestAmbiguousResponseHookOutcomeCannotBeReplayed(t *testing.T) {
	var calls atomic.Int64
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok { t.Error("HTTP hijack unavailable"); return }
		conn, _, err := hj.Hijack()
		if err != nil { t.Errorf("hijack failed: %v", err); return }
		_ = conn.Close() // request arrived, but no outcome reached Regate
	}))
	defer hook.Close()
	src := fmt.Sprintf(`hook "response_rewrite" {
  transport = "http"
  url = %q
  contract = "transform"
  timeout_ms = 5000
}
interceptor "response_filter" {
  stage = "before_response"
  hook = "response_rewrite"
}
`, hook.URL) + resumedResponseFixture()
	rt, cfg, id := suspendedResponseRun(t, src)
	out, err := rt.Resume(context.Background(), cfg, "rev", id, "pending")
	if err == nil || out.Status != "failure" || out.Value != nil {
		t.Fatalf("lost response hook did not fail closed: %#v %v", out, err)
	}
	var cp Checkpoint
	if err := rt.Store.GetSuspended(id, &cp); err != sql.ErrNoRows {
		t.Fatalf("completed execution checkpoint retained after ambiguous hook: %v", err)
	}
	if _, err = rt.Resume(context.Background(), cfg, "rev", id, "repeat"); err == nil || calls.Load() != 1 {
		t.Fatalf("unknown external effect replayed after resumed boundary failure: %v calls=%d", err, calls.Load())
	}
}
