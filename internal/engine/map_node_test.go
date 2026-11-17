package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func boundedMapPipeline() string {
	return `pipeline "batch" {
  entry = "upper_names"
  node "upper_names" {
    kind = "map"
    items = "input.names"
    mapping = "upper(input)"
    max_items = 2
    input_schema = {type = "string"}
    output_schema = {
      type = "array"
      items = {type = "string"}
    }
  }
  node "finish" {
    kind = "output"
    value = "nodes.upper_names.value"
  }
  edge {
    from = "upper_names"
    to = "finish"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "names" {
    pipeline = "batch"
    read_only = true
  }
}
`
}

func TestBoundedMapExecutesPureCELAndPreservesOrder(t *testing.T) {
	cfg, err := config.Decode(boundedMapPipeline())
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct{
		name string
		items []any
		want []any
	}{
		{"nonempty", []any{"alice","Bob"}, []any{"ALICE","BOB"}},
		{"empty", []any{}, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names", map[string]any{"names":tc.items}, true)
			if err != nil { t.Fatal(err) }
			if got.Status != "success" || !reflect.DeepEqual(got.Value, tc.want) {
				t.Fatalf("unexpected result %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestBoundedMapFailsClosedOnInvalidInputWithoutPartialOutput(t *testing.T) {
	cases := []struct{
		name, source string
		input any
		errorContains string
	}{
		{"too many", boundedMapPipeline(), []any{"one","two","three"}, "configured bound"},
		{"wrong items type", boundedMapPipeline(), "not-array", "must be an array"},
		{"item schema", boundedMapPipeline(), []any{"valid", 17}, "schema validation"},
		{"runtime CEL", boundedMapPipeline(), []any{"valid", nil}, "schema validation"},
		{"output schema", strings.Replace(boundedMapPipeline(), `items = {type = "string"}`, `items = {type = "integer"}`, 1), []any{"a"}, "node upper_names output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Decode(tc.source)
			if err != nil { t.Fatal(err) }
			got, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "names", map[string]any{"names":tc.input}, true)
			if err == nil || !strings.Contains(err.Error(), tc.errorContains) {
				t.Fatalf("wanted %q failure, got %#v, %v", tc.errorContains, got, err)
			}
			if got.Value != nil {
				t.Fatalf("failed map published partial result: %#v", got.Value)
			}
		})
	}
}
