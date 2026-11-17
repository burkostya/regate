package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func rootOutputExclusivitySource() string {
	return `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "left" {
    kind = "output"
    value = "literal:LEFT"
  }
  node "right" {
    kind = "output"
    value = "literal:RIGHT"
  }
  edge {
    from = "start"
    to = "left"
  }
  edge {
    from = "start"
    to = "right"
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

func TestRootSurfaceCompetingOutputsFailClosed(t *testing.T) {
	cfg, err := config.Decode(rootOutputExclusivitySource())
	if err != nil { t.Fatal(err) }
	for i := 0; i < 10; i++ {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, true)
		if err == nil || !strings.Contains(err.Error(), "competing outputs: left, right") {
			t.Fatalf("ambiguous output accepted: %#v, %v", out, err)
		}
		if out.Value != nil || out.MCP != nil || out.OutputProduced || out.Status != "failure" {
			t.Fatalf("partial output escaped: %#v", out)
		}
	}
}

func TestRootSurfaceMutuallyExclusiveOutputsStillWork(t *testing.T) {
	src := strings.Replace(rootOutputExclusivitySource(),
		`  node "left" {`, `  node "branch" {
    kind = "condition"
    test = "input.left"
  }
  node "left" {`, 1)
	src = strings.Replace(src, `    from = "start"
    to = "left"
  }`,
		`    from = "start"
    to = "branch"
  }
  edge {
    from = "branch"
    to = "left"
    when = "true"
  }`, 1)
	src = strings.Replace(src, `    from = "start"
    to = "right"
  }`,
		`    from = "branch"
    to = "right"
    when = "false"
  }`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct{ chosen bool; want string }{{true, "LEFT"}, {false, "RIGHT"}} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "root", "read",
			map[string]any{"left":tc.chosen}, true)
		if err != nil || out.Status != "success" || out.Value != tc.want {
			t.Fatalf("valid exclusive output %v rejected: %#v, %v", tc.chosen, out, err)
		}
	}
}

func TestResumeRootCompetingOutputsConsumesCheckpoint(t *testing.T) {
	src := `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "early" {
    kind = "output"
    value = "literal:EARLY"
  }
  node "pause" {
    kind = "suspend"
  }
  node "late" {
    kind = "output"
    value = "nodes.pause.value"
  }
  edge {
    from = "start"
    to = "early"
  }
  edge {
    from = "start"
    to = "pause"
  }
  edge {
    from = "pause"
    to = "late"
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
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	rt := New(store)
	first, err := rt.Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, false)
	if err == nil || first.Status != "suspended" {
		t.Fatalf("expected durable pending output, got %#v %v", first, err)
	}
	resumed, err := New(store).Resume(context.Background(), cfg, "rev", first.RunID, "LATE")
	if err == nil || !strings.Contains(err.Error(), "competing outputs: early, late") {
		t.Fatalf("ambiguous resumed result accepted: %#v, %v", resumed, err)
	}
	if resumed.Value != nil || resumed.OutputProduced || resumed.Status != "failure" {
		t.Fatalf("ambiguous resumed result published: %#v", resumed)
	}
	var leftover Checkpoint
	if err := store.GetSuspended(first.RunID, &leftover); err == nil {
		t.Fatalf("consumed ambiguous checkpoint remains resumable: %#v", leftover)
	}
}


func TestRootSurfaceWithoutCompletedOutputFailsClosed(t *testing.T) {
	// A valid graph may have conditional terminals with no matching edge.
	// Do not publish a successful implicit null when both routes are skipped.
	src := rootOutputExclusivitySource()
	src = strings.Replace(src, `    to = "left"
  }`, `    to = "left"
    when = "never"
  }`, 1)
	src = strings.Replace(src, `    to = "right"
  }`, `    to = "right"
    when = "never"
  }`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "produced no output") ||
		out.Status != "failure" || out.OutputProduced ||
		out.Value != nil || out.MCP != nil || out.Nodes != nil {
		t.Fatalf("absent root output was published: %#v %v", out, err)
	}
}

func TestRootSurfaceActualNullOutputRemainsPresent(t *testing.T) {
	src := rootOutputExclusivitySource()
	src = strings.Replace(src, `value = "literal:LEFT"`, `value = "null"`, 1)
	src = strings.Replace(src, `    to = "right"
  }`, `    to = "right"
    when = "never"
  }`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, true)
	value, present := out.NamedOutputs["left"]
	if err != nil || out.Status != "success" || !out.OutputProduced ||
		len(out.NamedOutputs) != 1 || !present || value != nil || out.Value != nil {
		t.Fatalf("valid JSON null output misclassified as absent: %#v %v", out, err)
	}
}

func TestRootResumeNoOutputConsumesCompletedCheckpoint(t *testing.T) {
	src := `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "pause" {
    kind = "suspend"
    output_schema = {type = "string"}
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
    when = "never"
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
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	first, err := New(store).Run(context.Background(), "rev", cfg, "root", "read", map[string]any{}, false)
	if err == nil || first.Status != "suspended" {
		t.Fatalf("expected resumable graph: %#v %v", first, err)
	}
	out, err := New(store).Resume(context.Background(), cfg, "rev", first.RunID, "accepted")
	if err == nil || !strings.Contains(err.Error(), "produced no output") ||
		out.Status != "failure" || out.OutputProduced ||
		out.Value != nil || out.Checkpoint != nil {
		t.Fatalf("missing resumed root terminal was published: %#v %v", out, err)
	}
	var cp Checkpoint
	if err := store.GetSuspended(first.RunID, &cp); err == nil {
		t.Fatal("fully executed missing-output continuation was not consumed")
	}
}
