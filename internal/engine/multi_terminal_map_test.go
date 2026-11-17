package engine

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func mappedMultiTerminalSource() string {
	return `pipeline "child" {
  entry = "start"
  timeout_ms = 2000
  node "start" {
    kind = "input"
    input_schema = {type = "object"}
  }
  node "left" {
    kind = "output"
    value = "input.left"
    output_schema = {type = "string"}
  }
  node "right" {
    kind = "output"
    value = "input.right"
    output_schema = {type = "string"}
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
pipeline "batch" {
  entry = "each"
  node "each" {
    kind = "map"
    items = "input.records"
    pipeline = "child"
    max_items = 3
    input_schema = {type = "object"}
    output_schema = {
      type = "array"
      items = {
        type = "object"
        required = ["left"]
        properties = {
          left = {type = "string"}
          right = {type = "string"}
        }
        additionalProperties = false
      }
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.each.value"
  }
  edge {
    from = "each"
    to = "done"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "run" {
    pipeline = "batch"
    read_only = true
  }
}
`
}

func TestMappedMultipleChildTerminalsHaveDeterministicPerItemObjects(t *testing.T) {
	cfg, err := config.Decode(mappedMultiTerminalSource())
	if err != nil {
		t.Fatal(err)
	}
	records := []any{
		map[string]any{"left": "A", "right": "B"},
		map[string]any{"left": "C", "right": "D"},
	}
	out, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
		map[string]any{"records": records}, true)
	if err != nil || out.Status != "success" || !reflect.DeepEqual(out.Value, records) {
		t.Fatalf("multi-output map changed ordering/values: %#v %v", out, err)
	}

	// An optional right terminal is omitted, not replaced by null.
	src := strings.Replace(mappedMultiTerminalSource(), `    to = "right"
  }`, `    to = "right"
    when = "never"
  }`, 1)
	cfg, err = config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err = New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
		map[string]any{"records": records}, true)
	want := []any{map[string]any{"left": "A"}, map[string]any{"left": "C"}}
	if err != nil || !reflect.DeepEqual(out.Value, want) {
		t.Fatalf("optional child terminal was not sparse: %#v %v", out, err)
	}
}

func TestMappedMultipleTerminalsSurviveSequentialSuspendResume(t *testing.T) {
	src := strings.Replace(mappedMultiTerminalSource(), `  edge {
    from = "start"
    to = "right"
  }
}
pipeline "batch" {`, `  edge {
    from = "start"
    to = "right"
  }
  node "pause" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  edge {
    from = "start"
    to = "pause"
  }
}
pipeline "batch" {`, 1)
	src = strings.Replace(src, `    pipeline = "child"
    max_items = 3`, `    pipeline = "child"
    allow_effects = true
    allow_suspend = true
    max_items = 3`, 1)
	cfg, err := config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	store := effectTestStore(t)
	args := map[string]any{"records": []any{
		map[string]any{"left": "A", "right": "B"},
		map[string]any{"left": "C", "right": "D"},
	}}
	first, err := New(store).Run(context.Background(), "rev", cfg, "mapped", "run", args, false)
	if err == nil || first.Status != "suspended" ||
		!reflect.DeepEqual(first.SuspensionPaths, [][]string{{"each", "0", "pause"}}) {
		t.Fatalf("first multi-terminal iteration did not suspend: %#v %v", first, err)
	}
	middle, err := New(store).ResumeAt(context.Background(), cfg, "rev", first.RunID,
		[]string{"each", "0", "pause"}, "accepted")
	if err == nil || middle.Status != "suspended" ||
		!reflect.DeepEqual(middle.SuspensionPaths, [][]string{{"each", "1", "pause"}}) {
		t.Fatalf("next multi-terminal iteration was lost: %#v %v", middle, err)
	}
	var cp Checkpoint
	if err := store.GetSuspended(first.RunID, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Child == nil || cp.Child.FrameKind != "map" || cp.Child.MapIndex != 1 ||
		!reflect.DeepEqual(cp.Child.MapResults, []any{map[string]any{"left": "A", "right": "B"}}) {
		t.Fatalf("typed multi-terminal prefix not preserved: %#v", cp.Child)
	}
	done, err := New(store).ResumeAt(context.Background(), cfg, "rev", first.RunID,
		[]string{"each", "1", "pause"}, "accepted")
	if err != nil || done.Status != "success" || !reflect.DeepEqual(done.Value, args["records"]) {
		t.Fatalf("resumed multi-terminal items missing: %#v %v", done, err)
	}
	if err := store.GetSuspended(first.RunID, &cp); err != sql.ErrNoRows {
		t.Fatalf("completed checkpoint not consumed: %v", err)
	}
}
