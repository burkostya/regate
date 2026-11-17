package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func dataPortPipeline() string {
	return `pipeline "ports" {
  entry = "produce"
  node "produce" {
    kind = "transform"
    value = "literal:ok"
    output_schema = {type = "string"}
  }
  node "finish" {
    kind = "output"
    value = "ports.result"
    input_ports = {
      result = {type = "string"}
    }
  }
  edge {
    from = "produce"
    to = "finish"
  }
  edge {
    kind = "data"
    from = "produce"
    to = "finish"
    to_port = "result"
  }
}
surface "ports" {
  path = "/mcp/ports"
  tool "echo" {
    pipeline = "ports"
    read_only = true
  }
}
`
}

func TestDataPortBindsNamedValue(t *testing.T) {
	cfg, err := config.Decode(dataPortPipeline())
	if err != nil { t.Fatal(err) }
	result, err := New(nil).Run(context.Background(), "rev", cfg, "ports", "echo", map[string]any{}, true)
	if err != nil { t.Fatal(err) }
	if result.Value != "ok" || result.Status != "success" {
		t.Fatalf("data binding lost: %#v", result)
	}
}

func TestDataPortDynamicSchemaFailsBeforeConsumer(t *testing.T) {
	source := strings.Replace(dataPortPipeline(), `    output_schema = {type = "string"}`, "", 1)
	source = strings.Replace(source, `result = {type = "string"}`, `result = {type = "integer"}`, 1)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) } // Producer is dynamic; runtime must refine.
	result, err := New(nil).Run(context.Background(), "rev", cfg, "ports", "echo", map[string]any{}, true)
	if err == nil || !strings.Contains(err.Error(), "schema validation") {
		t.Fatalf("invalid data passed the port boundary: %#v, %v", result, err)
	}
	if result.Value != nil {
		t.Fatalf("invalid value must not be published: %#v", result)
	}
}
