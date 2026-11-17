package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func partialOutputFailureFixture(suspend bool) string {
	// The early root output is complete before the terminal failure:
	// the first graph wave may otherwise leave an observable partial result.
	middle := `  node "middle" {
    kind = "transform"
    value = "literal:OK"
  }`
	connection := `  edge {
    from = "start"
    to = "early"
  }
  edge {
    from = "start"
    to = "middle"
  }`
	if suspend {
		middle = `  node "pause" {
    kind = "suspend"
  }
  node "middle" {
    kind = "transform"
    value = "nodes.pause.value"
  }`
		connection = `  edge {
    from = "start"
    to = "pause"
  }
  edge {
    from = "pause"
    to = "early"
  }
  edge {
    from = "pause"
    to = "middle"
  }`
	}
	return `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "early" {
    kind = "output"
    value = "literal:EARLY"
  }
`+middle+`
  node "bad" {
    kind = "output"
    value = "literal:NOT_INTEGER"
    output_schema = {type = "integer"}
  }
`+connection+`
  edge {
    from = "middle"
    to = "bad"
  }
}
surface "root" {
  path = "/mcp/root"
  tool "read" {
    pipeline = "root"
    read_only = true
  }
}
`
}

func TestFailedRootRunNeverReturnsEarlierCompletedOutput(t *testing.T) {
	cfg, err := config.Decode(partialOutputFailureFixture(false))
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("invalid later terminal did not fail: %#v, %v", out, err)
	}
	if out.Status != "failure" || out.Value != nil || out.OutputProduced ||
		out.MCP != nil || out.Nodes != nil || out.Checkpoint != nil {
		t.Fatalf("earlier successful output escaped failed run: %#v", out)
	}
}

func TestFailedResumeNeverReturnsEarlierPostContinuationOutput(t *testing.T) {
	cfg, err := config.Decode(partialOutputFailureFixture(true))
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	rt := New(store)
	first, err := rt.Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, false)
	if err == nil || first.Status != "suspended" || first.Checkpoint == nil {
		t.Fatalf("expected a durable resume boundary: %#v, %v", first, err)
	}
	resumed, err := rt.Resume(context.Background(), cfg, "rev", first.RunID, "OK")
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("later failure accepted after resume: %#v, %v", resumed, err)
	}
	if resumed.Status != "failure" || resumed.Value != nil || resumed.OutputProduced ||
		resumed.MCP != nil || resumed.Nodes != nil || resumed.Checkpoint != nil {
		t.Fatalf("earlier post-resume output escaped failure: %#v", resumed)
	}
	var cp Checkpoint
	if err := store.GetSuspended(first.RunID, &cp); err != nil {
		t.Fatalf("unfinished graph checkpoint cannot be reconciled: %v", err)
	}
	if cp.SuspendNode != "pause" {
		t.Fatalf("unexpected unfinished checkpoint: %#v", cp)
	}
}
