package engine

import (
    "context"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
)

func namedCallExecutableSource() string {
    return `pipeline "child" {
  entry = "start"
  timeout_ms = 1000
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

func TestNamedSubpipelinePortsRouteBothFields(t *testing.T) {
    cfg, err := config.Decode(namedCallExecutableSource())
    if err != nil { t.Fatal(err) }
    out, err := New(nil).Run(context.Background(), "rev", cfg, "named", "combine",
        map[string]any{"first": "Ada", "second": "Z"}, true)
    if err != nil { t.Fatal(err) }
    if out.Status != "success" || out.Value != "Ada:Z" {
        t.Fatalf("named child outputs not delivered: %#v", out)
    }
}

func TestNamedSubpipelinePortsFailClosedBeforeParentOutput(t *testing.T) {
    base := namedCallExecutableSource()
    cases := []struct{name, source, want string}{
        {
            "missing child field",
            strings.Replace(base, `value = {first = "input.first", second = "input.second"}`, `value = {first = "input.first"}`, 1),
            "JSON Schema",
        },
        {
            "parent port runtime refinement",
            strings.Replace(base, `output_ports = {first = {type = "string"}, second = {type = "string"}}`, `output_ports = {first = {type = "string"}, second = {type = "string", pattern = "^Z$"}}`, 1),
            "output port second failed schema validation",
        },
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            cfg, err := config.Decode(tc.source)
            if err != nil { t.Fatal(err) }
            out, err := New(nil).Run(context.Background(), "rev", cfg, "named", "combine",
                map[string]any{"first": "Ada", "second": "not-Z"}, true)
            if err == nil || !strings.Contains(err.Error(), tc.want) {
                t.Fatalf("expected %q, got output %#v and error %v", tc.want, out, err)
            }
            if out.Value != nil || out.OutputProduced {
                t.Fatalf("invalid child output escaped to parent: %#v", out)
            }
        })
    }
}
