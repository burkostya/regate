package config

import (
	"strings"
	"testing"
)

func boundedMapSource() string {
	return `pipeline "batch" {
  entry = "upper_names"
  node "upper_names" {
    kind      = "map"
    items     = "input.names"
    mapping   = "upper(input)"
    max_items = 2
    input_schema = {type = "string"}
    output_schema = {
      type  = "array"
      items = {type = "string"}
    }
  }
  node "finish" {
    kind  = "output"
    value = "nodes.upper_names.value"
  }
  edge {
    from = "upper_names"
    to   = "finish"
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

func TestBoundedMapSchemaAndIR(t *testing.T) {
	cfg, err := Decode(boundedMapSource())
	if err != nil { t.Fatal(err) }
	plan := cfg.Plans["batch"].Nodes["upper_names"]
	if plan.Expressions["items"] == nil || plan.Expressions["mapping"] == nil ||
		plan.InputSchema == nil || plan.OutputSchema == nil {
		t.Fatalf("map expression or schemas not compiled: %+v", plan)
	}
	if cfg.Pipelines["batch"].Nodes["upper_names"].Int("max_items", 0) != 2 {
		t.Fatal("max_items lost from parsed config")
	}
}

func TestBoundedMapRejectsInvalidDeclarations(t *testing.T) {
	base := boundedMapSource()
	tests := []struct{
		name, src, expect string
	}{
		{"missing items", strings.Replace(base, `    items     = "input.names"`, "", 1), "items"},
		{"missing mapping", strings.Replace(base, `    mapping   = "upper(input)"`, "", 1), "mapping"},
		{"missing bound", strings.Replace(base, `    max_items = 2`, "", 1), "max_items"},
		{"zero bound", strings.Replace(base, `max_items = 2`, `max_items = 0`, 1), "max_items"},
		{"too many items", strings.Replace(base, `max_items = 2`, `max_items = 65`, 1), "max_items"},
		{"noninteger bound", strings.Replace(base, `max_items = 2`, `max_items = "2"`, 1), "max_items"},
		{"invalid mapping CEL", strings.Replace(base, `mapping   = "upper(input)"`, `mapping   = "1 + true"`, 1), "CEL"},
		{"unknown node dependency", strings.Replace(base, `items     = "input.names"`, `items     = "nodes.unknown.value"`, 1), "missing node"},
		{"unknown port dependency", strings.Replace(base, `items     = "input.names"`, `items     = "ports.unknown"`, 1), "unknown input port"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.src); err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("expected error %q, got %v", tc.expect, err)
			}
		})
	}
}
