package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func TestParallelPurePipelineMapPreservesMultiTerminalAndOptionalOrder(t *testing.T) {
	source := strings.Replace(mappedMultiTerminalSource(), "max_items = 3",
		"max_items = 3\n    parallelism = 3", 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	records := []any{
		map[string]any{"left":"C","right":"D"},
		map[string]any{"left":"A","right":"B"},
		map[string]any{"left":"E","right":"F"},
	}
	for i := 0; i < 5; i++ {
		got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
			map[string]any{"records":records}, true)
		if err != nil || !reflect.DeepEqual(got.Value, records) {
			t.Fatalf("parallel pure child results out of order: %#v %v", got, err)
		}
	}
	// Each child may omit its optional right terminal. No item gets a
	// synthetic null field in the parallel aggregation.
	sparse := strings.Replace(source, `    to = "right"
  }`, `    to = "right"
    when = "never"
  }`, 1)
	cfg, err = config.Decode(sparse)
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
		map[string]any{"records":records}, true)
	want := []any{
		map[string]any{"left":"C"}, map[string]any{"left":"A"}, map[string]any{"left":"E"},
	}
	if err != nil || !reflect.DeepEqual(got.Value, want) {
		t.Fatalf("parallel optional child terminals changed presence: %#v %v", got, err)
	}
}

func TestParallelPurePipelineMapSelectsFirstChildErrorAndHidesPartialOutputs(t *testing.T) {
	source := strings.Replace(mappedMultiTerminalSource(), "max_items = 3",
		"max_items = 3\n    parallelism = 3", 1)
	source = strings.Replace(source, `value = "input.left"`, `value = "upper(input.left)"`, 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	records := []any{
		map[string]any{"left":"ok","right":"yes"},
		map[string]any{"left":42,"right":"other"},
		map[string]any{"left":43,"right":"another"},
	}
	for i := 0; i < 5; i++ {
		got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
			map[string]any{"records":records}, true)
		if err == nil || !strings.Contains(err.Error(), "map item 1:") ||
			got.Status != "failure" || got.Value != nil || got.MCP != nil {
			t.Fatalf("parallel child error not deterministic/fail-closed: %#v %v", got, err)
		}
	}
}

func TestParallelPurePipelineMapRejectsInvalidLaterItemBeforeChild(t *testing.T) {
	source := strings.Replace(mappedMultiTerminalSource(), "max_items = 3",
		"max_items = 3\n    parallelism = 3", 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "run",
		map[string]any{"records":[]any{map[string]any{"left":"A","right":"B"}, 42}}, true)
	if err == nil || !strings.Contains(err.Error(), "map item 1: pipeline argument must be an object") || got.Value != nil {
		t.Fatalf("parallel child input preflight leaked result: %#v %v", got, err)
	}
}
