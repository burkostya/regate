package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func allJoinPortPipeline() string {
	return `pipeline "pair" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "left" {
    kind = "transform"
    value = "literal:L"
    output_schema = {type = "string"}
  }
  node "right" {
    kind = "transform"
    value = "literal:R"
    output_schema = {type = "string"}
  }
  node "merge" {
    kind = "join"
    mode = "all"
    value = "ports.left + ports.right"
    input_ports = {
      left = {type = "string"}
      right = {type = "string"}
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.merge.value"
  }
  edge {
    from = "start"
    to = "left"
  }
  edge {
    from = "start"
    to = "right"
  }
  edge {
    from = "left"
    to = "merge"
  }
  edge {
    from = "right"
    to = "merge"
  }
  edge {
    from = "merge"
    to = "done"
  }
  edge {
    kind = "data"
    from = "left"
    to = "merge"
    to_port = "left"
  }
  edge {
    kind = "data"
    from = "right"
    to = "merge"
    to_port = "right"
  }
}
surface "pair" {
  path = "/mcp/pair"
  tool "merge" {
    pipeline = "pair"
    read_only = true
  }
}
`
}

func TestAllJoinBindsParallelProducerPorts(t *testing.T) {
	cfg, err := config.Decode(allJoinPortPipeline())
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "pair", "merge", map[string]any{}, true)
	if err != nil { t.Fatal(err) }
	if got.Value != "LR" || got.Status != "success" {
		t.Fatalf("all join did not bind both completed branches: %#v", got)
	}
}

func TestAllJoinSkippedBranchDoesNotPublishIncompleteData(t *testing.T) {
	src := strings.Replace(allJoinPortPipeline(), `    from = "right"
    to = "merge"`, `    from = "right"
    to = "merge"
    when = "true"`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "pair", "merge", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "produced no output") ||
		got.Value != nil || got.OutputProduced || got.Status != "failure" {
		t.Fatalf("skipped branch leaked incomplete output or was treated as successful: %#v %v", got, err)
	}
}
