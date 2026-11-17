package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

// Both child terminals settle before the independent pause. On Resume no
// output node runs again: the child must reconstruct presence from its
// durable terminal snapshot rather than the current execution wave.
func suspendedMultiTerminalSource() string {
	src := multiTerminalExecutableSource()
	src = strings.Replace(src, `  edge {
    from = "start"
    to = "right"
  }
}
pipeline "parent" {`, `  edge {
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
pipeline "parent" {`, 1)
	src = strings.Replace(src, `    arguments = "input"
    output_ports = {left = {type = "string"}, right = {type = "string"}}`, `    arguments = "input"
    allow_effects = true
    allow_suspend = true
    output_ports = {left = {type = "string"}, right = {type = "string"}}`, 1)
	return src
}

func TestMultiTerminalCallRestoresOutputsCompletedBeforeSuspend(t *testing.T) {
	cfg, err := config.Decode(suspendedMultiTerminalSource())
	if err != nil {
		t.Fatal(err)
	}
	store := effectTestStore(t)
	args := map[string]any{"left": "A", "right": "B"}
	first, err := New(store).Run(context.Background(), "rev", cfg, "terminal", "combine", args, false)
	if err == nil || first.Status != "suspended" {
		t.Fatalf("child did not suspend: %#v %v", first, err)
	}
	if want := [][]string{{"call", "pause"}}; !reflect.DeepEqual(first.SuspensionPaths, want) {
		t.Fatalf("unexpected pending child path: %#v", first.SuspensionPaths)
	}
	var cp Checkpoint
	if err := store.GetSuspended(first.RunID, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Child == nil || cp.Child.States["left"] != "completed" || cp.Child.States["right"] != "completed" {
		t.Fatalf("child output terminals not in durable snapshot: %#v", cp.Child)
	}
	done, err := New(store).Resume(context.Background(), cfg, "rev", first.RunID, "ack")
	if err != nil || done.Status != "success" || !done.OutputProduced || done.Value != "A:B" {
		t.Fatalf("restored child terminals were not published: %#v %v", done, err)
	}
	if err := store.GetSuspended(first.RunID, &cp); err == nil {
		t.Fatal("consumed checkpoint still exists")
	}
}

func TestMultiTerminalRootResumeStillRejectsCompetingOutputs(t *testing.T) {
	src := strings.Replace(suspendedMultiTerminalSource(),
		`    pipeline = "parent"`, `    pipeline = "child"`, 1)
	cfg, err := config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	store := effectTestStore(t)
	first, err := New(store).Run(context.Background(), "rev", cfg, "terminal", "combine",
		map[string]any{"left": "A", "right": "B"}, false)
	if err == nil || first.Status != "suspended" {
		t.Fatalf("competing root did not suspend: %#v %v", first, err)
	}
	done, err := New(store).Resume(context.Background(), cfg, "rev", first.RunID, "ack")
	if err == nil || !strings.Contains(err.Error(), "multiple") || done.Status != "failure" ||
		done.Value != nil || done.OutputProduced || done.MCP != nil {
		t.Fatalf("competing resumed root outputs were published: %#v %v", done, err)
	}
	var cp Checkpoint
	if err := store.GetSuspended(first.RunID, &cp); err == nil {
		t.Fatal("ambiguous completed checkpoint could be resumed again")
	}
}
