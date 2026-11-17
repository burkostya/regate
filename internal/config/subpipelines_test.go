package config

import (
	"fmt"
	"strings"
	"testing"
)

func purePipelineCallSource() string {
	return `pipeline "inner" {
  entry = "entry"
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
    output_schema = {type = "string"}
  }
  node "finish" {
    kind = "output"
    value = "nodes.upper.value"
    output_schema = {type = "string"}
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
    input_schema = {type = "object"}
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

func TestPurePipelineCallCompilationAndContracts(t *testing.T) {
	cfg, err := Decode(purePipelineCallSource())
	if err != nil { t.Fatal(err) }
	if cfg.Plans["outer"].Nodes["caller"].Expressions["arguments"] == nil {
		t.Fatal("child arguments were not compiled")
	}
	if cfg.Pipelines["outer"].Nodes["caller"].Str("pipeline") != "inner" {
		t.Fatal("child target was not retained")
	}
}

// selfRecursiveCall makes the caller an input-entry pipeline so the
// recursive call reaches cycle detection rather than failing entry checks.
func selfRecursiveCall(base string) string {
	source := strings.Replace(base, `pipeline = "inner"`, `pipeline = "outer"`, 1)
	source = strings.Replace(source, `  entry = "caller"`, `  entry = "start"`, 1)
	source = strings.Replace(source, `  node "caller" {`, `  node "start" {
    kind = "input"
  }
  node "caller" {`, 1)
	source = strings.Replace(source, `  edge {
    from = "caller"
    to = "finish"
  }`, `  edge {
    from = "start"
    to = "caller"
  }
  edge {
    from = "caller"
    to = "finish"
  }`, 1)
	return source
}

func TestPurePipelineCallRejectedUnsafeConfigurations(t *testing.T) {
	base := purePipelineCallSource()
	childOutput := `  node "finish" {
    kind = "output"
    value = "nodes.upper.value"
    output_schema = {type = "string"}
  }`
	childTransform := `  node "upper" {
    kind = "transform"
    value = "upper(nodes.entry.value.name)"
    output_schema = {type = "string"}
  }`
	blockedSuspend := `  node "upper" {
    kind = "suspend"
  }`
	cases := []struct {name, src, want string}{
		{"unknown pipeline", strings.Replace(base, `pipeline = "inner"`, `pipeline = "not_found"`, 1), "unknown called pipeline"},
		{"cycle", selfRecursiveCall(base), "cyclic pipeline.call"},
		{"suspending callee", strings.Replace(base, childTransform, blockedSuspend, 1), "suspend requires durable nested checkpoint support"},
		{"multiple outputs", strings.Replace(base, childOutput, childOutput + `
  node "second" {
    kind = "output"
    value = "nodes.upper.value"
  }
  edge {
    from = "upper"
    to = "second"
  }`, 1), "must declare output_ports for child terminal"},
		{"missing arguments", strings.Replace(base, `    arguments = {name = "input.name"}`, "", 1), "arguments"},
		{"nonobject arguments", strings.Replace(base, `arguments = {name = "input.name"}`, `arguments = ["input.name"]`, 1), "arguments"},
		{"incompatible callee input", strings.Replace(base, `input_schema = {type = "object"}`, `input_schema = {type = "string"}`, 1), "incompatible port types"},
		{"incompatible callee output", strings.Replace(base, `value = "nodes.upper.value"
    output_schema = {type = "string"}`, `value = "nodes.upper.value"
    output_schema = {type = "integer"}`, 1), "incompatible port types"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.src); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func nestedPureCallChain(count int) string {
	var b strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, `pipeline "p%d" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
`, i)
		if i+1 < count {
			fmt.Fprintf(&b, `  node "child" {
    kind = "pipeline.call"
    pipeline = "p%d"
    arguments = "nodes.entry.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.child.value"
  }
  edge {
    from = "entry"
    to = "child"
  }
  edge {
    from = "child"
    to = "done"
  }
}
`, i+1)
		} else {
			b.WriteString(`  node "done" {
    kind = "output"
    value = "nodes.entry.value"
  }
  edge {
    from = "entry"
    to = "done"
  }
}
`)
		}
	}
	return b.String()
}

func TestPurePipelineCallDepthBound(t *testing.T) {
	if _, err := Decode(nestedPureCallChain(MaxPurePipelineCallDepth)); err != nil {
		t.Fatalf("max allowed depth rejected: %v", err)
	}
	if _, err := Decode(nestedPureCallChain(MaxPurePipelineCallDepth+1)); err == nil ||
		!strings.Contains(err.Error(), "depth exceeds") {
		t.Fatalf("unbounded chain accepted: %v", err)
	}
}
