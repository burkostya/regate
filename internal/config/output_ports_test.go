package config

import (
	"strings"
	"testing"
)

func structuredDataPortSource() string {
	return `upstream "stub" {
  url = "http://127.0.0.1:9099/mcp"
}
pipeline "read" {
  entry = "lookup"
  node "lookup" {
    kind = "mcp.call"
    upstream = "stub"
    tool = "get"
    effect = "read"
    arguments = {id = "input.id"}
    mock = {message = "plain"}
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
      }
    }
  }
  edge {
    from = "lookup"
    to = "finish"
  }
  edge {
    kind = "data"
    from = "lookup"
    from_port = "structured"
    to = "finish"
    to_port = "customer"
  }
}
surface "mcp_structured" {
  path = "/mcp/structured"
  tool "read" {
    pipeline = "read"
    read_only = true
  }
}
`
}

func TestDeclaredStructuredMCPPortCompiles(t *testing.T) {
	cfg, err := Decode(structuredDataPortSource())
	if err != nil { t.Fatal(err) }
	plan := cfg.Plans["read"].Nodes["lookup"]
	if plan.OutputPorts["structured"] == nil {
		t.Fatal("structured output port lost during compilation")
	}
	edge := cfg.Pipelines["read"].Edges[1]
	if edge.FromPort != "structured" || edge.ToPort != "customer" {
		t.Fatalf("named data edge not retained: %#v", edge)
	}
}

func TestStructuredPortInvalidSourcesAreRejected(t *testing.T) {
	base := structuredDataPortSource()
	portDeclaration := `    output_ports = {
      structured = {
        type = "object"
        required = ["code"]
        properties = {code = {type = "string"}}
      }
    }`
	cases := []struct{name, src, want string}{
		{"undeclared source", strings.Replace(base, portDeclaration, "", 1), "requires a declared MCP structured output port"},
		{"wrong node kind", strings.Replace(base, `kind = "mcp.call"`, `kind = "transform"`, 1), "requires a declared MCP structured output port"},
		{"unsupported source port", strings.Replace(base, `from_port = "structured"`, `from_port = "metadata"`, 1), "invalid data edge"},
		{"known incompatible schemas", strings.Replace(base, `customer = {
        type = "object"`, `customer = {
        type = "string"`, 1), "incompatible port types"},
		{"invalid producer JSON schema", strings.Replace(base, `structured = {
        type = "object"`, `structured = {
        type = "not_a_json_type"`, 1), "output port structured"},
		{"extra unsupported output name", strings.Replace(base, `    output_ports = {`, `    output_ports = {
      raw_envelope = {type = "object"}`, 1), "unsupported output port raw_envelope"},
		{"wrong structured port contract shape", strings.Replace(base, `structured = {
        type = "object"
        required = ["code"]
        properties = {code = {type = "string"}}
      }`, `structured = "not-an-object"`, 1), "JSON Schema object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.src); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValueOutputPortRejectsDisjointDeclaredSchema(t *testing.T) {
	src := `pipeline "v" {
  entry = "start"
  node "start" {
    kind = "transform"
    value = "literal:one"
    output_schema = {type = "string"}
    output_ports = {value = {type = "integer"}}
  }
}
`
	if _, err := Decode(src); err == nil || !strings.Contains(err.Error(), "incompatible port types") {
		t.Fatalf("incompatible value output contract accepted: %v", err)
	}
}
