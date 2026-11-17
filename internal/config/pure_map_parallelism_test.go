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

func TestPureMapParallelismRejectsPipelineMode(t *testing.T) {
	base := `pipeline "child" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "end" {
    kind = "output"
    value = "nodes.start.value"
  }
  edge {
    from = "start"
    to = "end"
  }
}
pipeline "main" {
  entry = "batch"
  node "batch" {
    kind = "map"
    items = "input.items"
    pipeline = "child"
    max_items = 2
    parallelism = 2
    input_schema = {type = "object"}
    output_schema = {type = "array", items = {type = "object"}}
  }
  node "end" {
    kind = "output"
    value = "nodes.batch.value"
  }
  edge {
    from = "batch"
    to = "end"
  }
}
surface "s" {
  path = "/s"
  tool "t" {
    pipeline = "main"
  }
}
`
	if _, err := Decode(base); err == nil || !strings.Contains(err.Error(), "parallelism is supported only for pure CEL mapping") {
		t.Fatalf("parallel pipeline map must be rejected: %v", err)
	}
}
