package engine

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func TestParallelPureMapPreservesInputOrderAndEmptyArrays(t *testing.T) {
	src := strings.Replace(boundedMapPipeline(), "max_items = 2", "max_items = 64\n    parallelism = 8", 1)
	cfg, err := config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	items := make([]any, 64)
	expected := make([]any, 64)
	for i := range items {
		items[i] = fmt.Sprintf("name%02d", i)
		expected[i] = fmt.Sprintf("NAME%02d", i)
	}
	for repeat := 0; repeat < 6; repeat++ {
		got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names", map[string]any{"names": items}, true)
		if err != nil {
			t.Fatalf("run %d: %v", repeat, err)
		}
		if got.Status != "success" || !reflect.DeepEqual(got.Value, expected) {
			t.Fatalf("run %d: out-of-order or incomplete result: %#v", repeat, got.Value)
		}
	}
	got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names", map[string]any{"names": []any{}}, true)
	if err != nil || !reflect.DeepEqual(got.Value, []any{}) {
		t.Fatalf("empty parallel map: %#v, %v", got, err)
	}
}

func TestParallelPureMapRejectsEntireResultWithDeterministicFirstError(t *testing.T) {
	src := strings.Replace(boundedMapPipeline(), "max_items = 2", "max_items = 4\n    parallelism = 4", 1)
	src = strings.Replace(src, "upper(input)", "int(input)", 1)
	cfg, err := config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names",
			map[string]any{"names": []any{"10", "bad", "also-bad", "20"}}, true)
		if err == nil || !strings.Contains(err.Error(), "map item 1:") {
			t.Fatalf("run %d: expected deterministic first failing item: %v", i, err)
		}
		if out.Status != "failure" || out.Value != nil || out.MCP != nil {
			t.Fatalf("run %d: partial result leaked: %#v", i, out)
		}
	}
}

func TestParallelPureMapPreflightRejectsLaterInvalidItems(t *testing.T) {
	src := strings.Replace(boundedMapPipeline(), "max_items = 2", "max_items = 4\n    parallelism = 4", 1)
	cfg, err := config.Decode(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names",
		map[string]any{"names": []any{"first", 42, "third"}}, true)
	if err == nil || !strings.Contains(err.Error(), "map item 1 failed schema validation") || out.Value != nil {
		t.Fatalf("invalid parallel preflight must fail closed: %#v %v", out, err)
	}
}
