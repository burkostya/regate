package config

import (
    "strings"
    "testing"
)

func multiTerminalContractSource() string {
    return `pipeline "child" {
  entry = "start"
  timeout_ms = 1000
  node "start" {
    kind = "input"
  }
  node "left" {
    kind = "output"
    value = "input.left"
    output_schema = {type = "string"}
  }
  node "right" {
    kind = "output"
    value = "input.right"
    output_schema = {type = "string"}
  }
  edge {
    from = "start"
    to = "left"
  }
  edge {
    from = "start"
    to = "right"
  }
}
pipeline "parent" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "child"
    arguments = "input"
    output_ports = {left = {type = "string"}, right = {type = "string"}}
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
    from_port = "left"
    to = "done"
    to_port = "left"
  }
  edge {
    kind = "data"
    from = "call"
    from_port = "right"
    to = "done"
    to_port = "right"
  }
}
surface "terminal" {
  path = "/mcp/terminal"
  tool "combine" {
    pipeline = "parent"
    read_only = true
  }
}
`
}

func TestMultiTerminalChildCompilesWithExplicitContracts(t *testing.T) {
    cfg, err := Decode(multiTerminalContractSource())
    if err != nil { t.Fatal(err) }
    if outputNodes(cfg.Pipelines["child"]) != 2 {
        t.Fatalf("expected two child terminals")
    }
    for _, name := range []string{"left", "right"} {
        if cfg.Plans["parent"].Nodes["call"].OutputPorts[name] == nil {
            t.Fatalf("missing terminal contract %q", name)
        }
    }
}

func TestMultiTerminalChildRejectsAmbiguousInterfaces(t *testing.T) {
    src := multiTerminalContractSource()
    ports := `output_ports = {left = {type = "string"}, right = {type = "string"}}`
    right := `  node "right" {
    kind = "output"
    value = "input.right"
    output_schema = {type = "string"}
  }`
    cases := []struct{name, source, want string}{
        {"missing output port", strings.Replace(src, ports, `output_ports = {left = {type = "string"}}`, 1), "must declare output_ports for child terminal right"},
        {"invented output", strings.Replace(src, ports, `output_ports = {left = {type = "string"}, right = {type = "string"}, extra = {type = "string"}}`, 1), "undeclared child terminal extra"},
        {"child untyped", strings.Replace(src, right, strings.Replace(right, `    output_schema = {type = "string"}
`, "", 1), 1), "child terminal right requires output_schema"},
        {"incompatible field", strings.Replace(src, ports, `output_ports = {left = {type = "string"}, right = {type = "integer"}}`, 1), "incompatible port types"},
        {"reserved value terminal", strings.Replace(strings.Replace(src, `node "right"`, `node "value"`, 1), `to = "right"`, `to = "value"`, 1), "cannot name a terminal value"},
        {"incompatible aggregate", strings.Replace(src, `output_ports = {left = {type = "string"}, right = {type = "string"}}`, `output_schema = {type = "array"}
    `+ports, 1), "aggregate output contract"},
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
                t.Fatalf("expected %q, got %v", tc.want, err)
            }
        })
    }
}
