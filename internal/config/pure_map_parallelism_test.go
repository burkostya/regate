package config

import (
	"strings"
	"testing"
)

func TestPureMapParallelismContract(t *testing.T) {
	base := boundedMapSource()
	source := strings.Replace(base, "max_items = 2", "max_items = 2\n    parallelism = 2", 1)
	cfg, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pipelines["batch"].Nodes["upper_names"].Int("parallelism", 0); got != 2 {
		t.Fatalf("lost parallelism declaration: %d", got)
	}

	tests := []struct {
		name, source, want string
	}{
		{"zero", strings.Replace(source, "parallelism = 2", "parallelism = 0", 1), "parallelism"},
		{"negative", strings.Replace(source, "parallelism = 2", "parallelism = -2", 1), "parallelism"},
		{"wrong type", strings.Replace(source, "parallelism = 2", "parallelism = \"2\"", 1), "parallelism"},
		{"exceeds items", strings.Replace(source, "parallelism = 2", "parallelism = 3", 1), "parallelism"},
		{"above hard cap", strings.Replace(source, "max_items = 2", "max_items = 64", 1), ""},
	}
	for _, tc := range tests {
		if tc.name == "above hard cap" {
			tc.source = strings.Replace(tc.source, "parallelism = 2", "parallelism = 17", 1)
			tc.want = "parallelism"
		}
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestPurePipelineMapParallelismAllowsOnlyPureChildren(t *testing.T) {
	base := strings.Replace(mappedSubpipelineSource(),
		"max_items = 2", "max_items = 2\n    parallelism = 2", 1)
	cfg, err := Decode(base)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Pipelines["batch"].Nodes["each"].Int("parallelism", 0); got != 2 {
		t.Fatalf("parallel pure subpipeline map lost limit: %d", got)
	}
	for _, tc := range []struct {
		name, source, want string
	}{
		{"effect opt-in", strings.Replace(base, "parallelism = 2", "parallelism = 2\n    allow_effects = true", 1), "parallelism requires an effect-free"},
		{"suspend opt-in", strings.Replace(base, "parallelism = 2", "parallelism = 2\n    allow_effects = true\n    allow_suspend = true", 1), "parallelism requires an effect-free"},
		{"too many workers", strings.Replace(base, "parallelism = 2", "parallelism = 3", 1), "parallelism"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}

	// The transitive pure-call checker must still reject an actual effect
	// anywhere inside a supposedly pure concurrently mapped child.
	effectful := strings.Replace(base,
		`  node "done" {
    kind = "output"
    value = "nodes.start.value"
    output_schema = {type = "object"}
  }`,
		`  node "write" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.start.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.start.value"
    output_schema = {type = "object"}
  }`, 1)
	effectful = strings.Replace(effectful,
		`  edge {
    from = "start"
    to = "done"
  }`,
		`  edge {
    from = "start"
    to = "write"
  }
  edge {
    from = "write"
    to = "done"
  }`, 1)
	effectful = `upstream "crm" {
  url = "http://localhost:1234"
}` + "\n" + effectful
	if _, err := Decode(effectful); err == nil || !strings.Contains(err.Error(), "cannot execute inside a pure") {
		t.Fatalf("transitive effect inside parallel pure child accepted: %v", err)
	}
}
