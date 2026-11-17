package config

import (
	"strings"
	"testing"
)

func dataPortHCL() string {
	return `pipeline "ports" {
  entry = "produce"
  node "produce" {
    kind = "transform"
    value = "literal:ok"
    output_schema = {type = "string"}
  }
  node "finish" {
    kind = "output"
    value = "ports.text"
    input_ports = {
      text = {type = "string"}
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
    from_port = "value"
    to_port = "text"
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

func TestNamedDataPortCompilesWithControlSeparate(t *testing.T) {
	cfg, err := Decode(dataPortHCL())
	if err != nil { t.Fatal(err) }
	if got := cfg.Plans["ports"].Order; len(got) != 2 || got[0] != "produce" || got[1] != "finish" {
		t.Fatalf("data edge must not schedule a separate activation: %v", got)
	}
	plan := cfg.Plans["ports"].Nodes["finish"]
	if plan.InputPorts["text"] == nil || len(plan.Dependencies) != 1 || plan.Dependencies[0] != "produce" {
		t.Fatalf("missing typed data dependency: %+v", plan)
	}
	if cfg.Pipelines["ports"].Edges[1].Kind != "data" {
		t.Fatal("kind was not retained for semantic diff and source patching")
	}
}

func TestDataPortsFailClosedAtActivation(t *testing.T) {
	base := dataPortHCL()
	dataEdge := `  edge {
    kind = "data"
    from = "produce"
    to = "finish"
    from_port = "value"
    to_port = "text"
  }`
	controlEdge := `  edge {
    from = "produce"
    to = "finish"
  }`
	if !strings.Contains(base, dataEdge) || !strings.Contains(base, controlEdge) {
		t.Fatal("fixture changed")
	}
	bypass := strings.Replace(base, `entry = "produce"`, `entry = "start"`, 1)
	bypass = strings.Replace(bypass, `  node "produce" {`, `  node "start" {
    kind = "input"
  }
  node "produce" {`, 1)
	bypass = strings.Replace(bypass, controlEdge, `  edge {
    from = "start"
    to = "produce"
  }
  edge {
    from = "start"
    to = "finish"
  }`, 1)

	cases := []struct {
		name, source, want string
	}{
		{"data is not control", strings.Replace(base, controlEdge, "", 1), "unreachable"},
		{"missing binding", strings.Replace(base, dataEdge, "", 1), "unbound input port"},
		{"unknown source kind", strings.Replace(base, `kind = "data"`, `kind = "unknown"`, 1), "invalid control edge"},
		{"guarded data edge", strings.Replace(base, `    from_port = "value"`, `    from_port = "value"
    when = "true"`, 1), "invalid data edge"},
		{"undeclared structured output port", strings.Replace(base, `from_port = "value"`, `from_port = "structured"`, 1), "requires a declared MCP structured output port"},
		{"invalid identifier", strings.Replace(base, `to_port = "text"`, `to_port = "bad-name"`, 1), "invalid data edge"},
		{"undeclared port", strings.Replace(base, `to_port = "text"`, `to_port = "missing"`, 1), "undeclared data port"},
		{"no declaration", strings.Replace(base, `    input_ports = {
      text = {type = "string"}
    }`, "", 1), "requires declared input_ports"},
		{"conflicting producers", strings.Replace(base, dataEdge, dataEdge+"\n"+dataEdge, 1), "duplicate data port"},
		{"static incompatible schema", strings.Replace(base, `text = {type = "string"}`, `text = {type = "integer"}`, 1), "incompatible port types"},
		{"missing CEL port", strings.Replace(base, `value = "ports.text"`, `value = "ports.unknown"`, 1), "unknown input port"},
		{"producer bypass", bypass, "not guaranteed to complete"},
		{"control misuses port", strings.Replace(base, controlEdge, `  edge {
    kind = "control"
    from = "produce"
    to = "finish"
    to_port = "text"
  }`, 1), "invalid control edge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
