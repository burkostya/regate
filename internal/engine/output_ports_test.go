package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func structuredPortPipeline() string {
	return `upstream "stub" {
  url = "http://127.0.0.1:9099/mcp"
}
pipeline "read" {
  entry = "fetch"
  node "fetch" {
    kind = "mcp.call"
    upstream = "stub"
    tool = "lookup"
    effect = "read"
    arguments = {id = "input.id"}
    mock = {display = "ordinary"}
    mock_structured = {code = "C123"}
    output_ports = {
      structured = {
        type = "object"
        required = ["code"]
        properties = {code = {type = "string"}}
      }
    }
  }
  node "finish" {
    kind = "output"
    value = "ports.customer.code"
    input_ports = {
      customer = {
        type = "object"
        required = ["code"]
        properties = {code = {type = "string"}}
      }
    }
  }
  edge {
    from = "fetch"
    to = "finish"
  }
  edge {
    kind = "data"
    from = "fetch"
    to = "finish"
    from_port = "structured"
    to_port = "customer"
  }
}
surface "structured" {
  path = "/mcp/structured"
  tool "lookup" {
    pipeline = "read"
    read_only = true
  }
}
`
}

func TestMCPStructuredDataPortDryRun(t *testing.T) {
	cfg, err := config.Decode(structuredPortPipeline())
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "structured", "lookup", map[string]any{"id":"C123"}, true)
	if err != nil { t.Fatal(err) }
	if out.Status != "success" || out.Value != "C123" {
		t.Fatalf("structured data not routed via port: %#v", out)
	}
}

func TestMCPStructuredPortFailsClosedWithoutResponseOrSchemaMatch(t *testing.T) {
	src := structuredPortPipeline()
	for _, tc := range []struct{name, source, want string}{
		{"absent structuredContent", strings.Replace(src, `    mock_structured = {code = "C123"}`, "", 1), "missing structured MCP output"},
		{"producer declared schema", strings.Replace(src, `mock_structured = {code = "C123"}`, `mock_structured = {code = 42}`, 1), "output port structured failed schema validation"},
		{"consumer required field", strings.Replace(src, `    mock_structured = {code = "C123"}`, `    mock_structured = {code = "C123", extra = true}`, 1), ""},
	} {
		if tc.want == "" {continue}
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Decode(tc.source)
			if err != nil { t.Fatal(err) }
			out, err := New(nil).Run(context.Background(), "rev", cfg, "structured", "lookup", map[string]any{"id":"C123"}, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %#v, %v", tc.want, out, err)
			}
			if out.Value != nil {
				t.Fatalf("invalid structured result published: %#v", out.Value)
			}
		})
	}
}
