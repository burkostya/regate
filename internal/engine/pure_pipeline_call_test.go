package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func pureCallExecutable() string {
	return `pipeline "inner" {
  entry = "entry"
  timeout_ms = 1000
  node "entry" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["name"]
      properties = {name = {type = "string"}}
    }
  }
  node "upper" {
    kind = "transform"
    value = "upper(nodes.entry.value.name)"
  }
  node "finish" {
    kind = "output"
    value = "nodes.upper.value"
  }
  edge {
    from = "entry"
    to = "upper"
  }
  edge {
    from = "upper"
    to = "finish"
  }
}
pipeline "outer" {
  entry = "caller"
  node "caller" {
    kind = "pipeline.call"
    pipeline = "inner"
    arguments = {name = "input.name"}
    output_schema = {type = "string"}
  }
  node "finish" {
    kind = "output"
    value = "nodes.caller.value"
  }
  edge {
    from = "caller"
    to = "finish"
  }
}
surface "nested" {
  path = "/mcp/nested"
  tool "call" {
    pipeline = "outer"
    read_only = true
  }
}
`
}

func TestPurePipelineCallInvokesNamedChild(t *testing.T) {
	cfg, err := config.Decode(pureCallExecutable())
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "nested", "call", map[string]any{"name":"alice"}, true)
	if err != nil { t.Fatal(err) }
	if got.Status != "success" || got.Value != "ALICE" || got.RunID == "" {
		t.Fatalf("nested result was not returned under outer run: %#v", got)
	}
}

func TestPurePipelineCallNoOutputIsError(t *testing.T) {
	source := strings.Replace(pureCallExecutable(), `    from = "upper"
    to = "finish"
  }`, `    from = "upper"
    to = "finish"
    when = "false"
  }`, 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "nested", "call", map[string]any{"name":"alice"}, true)
	if err == nil || !strings.Contains(err.Error(), "did not produce an output") {
		t.Fatalf("missing child output accepted: %#v %v", got, err)
	}
	if got.Value != nil { t.Fatal("incomplete nested pipeline published output") }
}

func TestPurePipelineCallInputValidation(t *testing.T) {
	cfg, err := config.Decode(pureCallExecutable())
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "nested", "call", map[string]any{"name":123}, true)
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("invalid nested input accepted: %#v %v", got, err)
	}
}

func TestPurePipelineCallOutputValidation(t *testing.T) {
	source := strings.Replace(pureCallExecutable(), `    output_schema = {type = "string"}`, `    output_schema = {type = "integer"}`, 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) } // Child output is dynamic, refine at runtime.
	got, err := New(nil).Run(context.Background(), "rev", cfg, "nested", "call", map[string]any{"name":"alice"}, true)
	if err == nil || !strings.Contains(err.Error(), "JSON Schema") {
		t.Fatalf("invalid nested output accepted: %#v %v", got, err)
	}
}
