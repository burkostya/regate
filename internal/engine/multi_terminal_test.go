package engine

import (
    "context"
    "reflect"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
)

func multiTerminalExecutableSource() string {
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

func TestMultiTerminalChildAggregatesAllOutputs(t *testing.T) {
    cfg, err := config.Decode(multiTerminalExecutableSource())
    if err != nil { t.Fatal(err) }
    out, err := New(nil).Run(context.Background(), "rev", cfg, "terminal", "combine",
        map[string]any{"left":"A", "right":"B"}, true)
    if err != nil { t.Fatal(err) }
    if out.Status != "success" || out.Value != "A:B" {
        t.Fatalf("multi-output routing failed: %#v", out)
    }
    // The legacy default value is now an aggregate for a multi-terminal child,
    // not the output of whichever parallel branch finishes last.
    src := strings.Replace(multiTerminalExecutableSource(),
        `value = "ports.left + ':' + ports.right"`,
        `value = "nodes.call.value"`, 1)
    cfg, err = config.Decode(src)
    if err != nil { t.Fatal(err) }
    out, err = New(nil).Run(context.Background(), "rev", cfg, "terminal", "combine",
        map[string]any{"left":"A", "right":"B"}, true)
    if err != nil { t.Fatal(err) }
    if want := (map[string]any{"left":"A","right":"B"}); !reflect.DeepEqual(out.Value, want) {
        t.Fatalf("ambiguous last-terminal value: got %#v, want %#v", out.Value, want)
    }
}

func TestMultiTerminalChildRejectsSkippedOrInvalidOutputs(t *testing.T) {
    src := multiTerminalExecutableSource()
    cases := []struct{name, source, want string}{
        {
            "skipped right terminal",
            strings.Replace(src, `    to = "right"
  }`, `    to = "right"
    when = "never"
  }`, 1),
            "missing required child output right",
        },
        {
            "invalid right terminal value",
            strings.Replace(src, `value = "input.right"`, `value = "input.unknown"`, 1),
            "JSON Schema",
        },
        {
            "caller output contract refinement",
            strings.Replace(src, `output_ports = {left = {type = "string"}, right = {type = "string"}}`,
                `output_ports = {left = {type = "string"}, right = {type = "string", pattern = "^Z$"}}`, 1),
            "output port right failed schema validation",
        },
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            cfg, err := config.Decode(tc.source)
            if err != nil { t.Fatal(err) }
            out, err := New(nil).Run(context.Background(), "rev", cfg, "terminal", "combine",
                map[string]any{"left":"A", "right":"B"}, true)
            if err == nil || !strings.Contains(err.Error(), tc.want) {
                t.Fatalf("expected %q, got %#v, %v", tc.want, out, err)
            }
            if out.OutputProduced || out.Value != nil {
                t.Fatalf("partial output escaped child boundary: %#v", out)
            }
        })
    }
}
