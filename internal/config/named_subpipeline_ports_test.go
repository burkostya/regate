package config

import (
    "strings"
    "testing"
)

// One child output object publishes two independently typed, required fields.
// The parent's ordinary value remains the entire object for backward
// compatibility; named data edges bind only the declared fields.
func namedCallContractSource() string {
    return `pipeline "child" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "done" {
    kind = "output"
    value = {first = "input.first", second = "input.second"}
    output_schema = {type = "object", required = ["first", "second"], properties = {first = {type = "string"}, second = {type = "string"}}}
  }
  edge {
    from = "start"
    to = "done"
  }
}
pipeline "parent" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "child"
    arguments = "input"
    output_ports = {first = {type = "string"}, second = {type = "string"}}
  }
  node "done" {
    kind = "output"
    value = "ports.left + ':' + ports.right"
    input_ports = {left = {type = "string"}, right = {type = "string"}}
  }
  edge {
    from = "call"
    to = "done"
  }
  edge {
    kind = "data"
    from = "call"
    from_port = "first"
    to = "done"
    to_port = "left"
  }
  edge {
    kind = "data"
    from = "call"
    from_port = "second"
    to = "done"
    to_port = "right"
  }
}
surface "named" {
  path = "/mcp/named"
  tool "combine" {
    pipeline = "parent"
    read_only = true
  }
}
`
}

func TestNamedSubpipelinePortsCompile(t *testing.T) {
    cfg, err := Decode(namedCallContractSource())
    if err != nil { t.Fatal(err) }
    for _, port := range []string{"first", "second"} {
        if cfg.Plans["parent"].Nodes["call"].OutputPorts[port] == nil {
            t.Fatalf("missing compiled call output port %s", port)
        }
    }
}

func TestNamedSubpipelinePortsRejectUnsafeContracts(t *testing.T) {
    src := namedCallContractSource()
    childSchema := `output_schema = {type = "object", required = ["first", "second"], properties = {first = {type = "string"}, second = {type = "string"}}}`
    ports := `output_ports = {first = {type = "string"}, second = {type = "string"}}`
    cases := []struct{name, source, want string}{
        {"missing call port", strings.Replace(src, ports, `output_ports = {second = {type = "string"}}`, 1), "requires declared pipeline.call output port first"},
        {"unmapped call port", strings.Replace(src, `from_port = "second"`, `from_port = "unknown"`, 1), "requires declared pipeline.call output port unknown"},
        {"child schema absent", strings.Replace(src, childSchema, "", 1), "requires child object output_schema"},
        {"child property not required", strings.Replace(src, `required = ["first", "second"]`, `required = ["first"]`, 1), "must require named output second"},
        {"child property missing", strings.Replace(src, childSchema, `output_schema = {type = "object", required = ["first", "second"], properties = {first = {type = "string"}}}`, 1), "must require named output second"},
        {"known incompatible type", strings.Replace(src, ports, `output_ports = {first = {type = "string"}, second = {type = "integer"}}`, 1), "incompatible port types"},
        {"forbidden computed source port", strings.Replace(src, `from_port = "second"`, `from_port = "bad-port"`, 1), "invalid data edge"},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
                t.Fatalf("expected %q, got %v", tc.want, err)
            }
        })
    }
}
