package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func optionalPortPipeline() string {
	return `pipeline "branches" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "predicate" {
    kind = "condition"
    test = "input.include_left"
  }
  node "left" {
    kind = "transform"
    value = "literal:LEFT"
    output_schema = {type = "string"}
  }
  node "right" {
    kind = "transform"
    value = "literal:RIGHT"
  }
  node "merge" {
    kind = "join"
    mode = "any"
    input_ports = {
      left = {
        optional = true
        schema = {type = ["string", "null"]}
      }
    }
    value = "port_present.left ? ports.left : 'fallback'"
  }
  node "done" {
    kind = "output"
    value = "nodes.merge.value"
  }
  edge {
    from = "start"
    to = "predicate"
  }
  edge {
    from = "start"
    to = "right"
  }
  edge {
    from = "predicate"
    to = "left"
    when = "true"
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
}
surface "optional" {
  path = "/mcp/optional"
  tool "merge" {
    pipeline = "branches"
    read_only = true
  }
}
`
}

func TestOptionalPortHandlesBothCompletedAndSkippedBranches(t *testing.T) {
	cfg, err := config.Decode(optionalPortPipeline())
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct{
		name string
		activate bool
		want string
	}{
		{"completed", true, "LEFT"},
		{"skipped", false, "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "merge",
				map[string]any{"include_left":tc.activate}, true)
			if err != nil { t.Fatal(err) }
			if got.Value != tc.want || got.Status != "success" {
				t.Fatalf("unexpected optional result %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestOptionalPortDistinguishesCompletedNullFromSkipped(t *testing.T) {
	src := strings.Replace(optionalPortPipeline(), `value = "literal:LEFT"
    output_schema = {type = "string"}`, `value = "null"`, 1)
	src = strings.Replace(src, `value = "port_present.left ? ports.left : 'fallback'"`,
		`value = "port_present.left ? (ports.left == null ? 'NULL' : ports.left) : 'ABSENT'"`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct{
		included bool
		want string
	}{
		{true, "NULL"},
		{false, "ABSENT"},
	} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "merge",
			map[string]any{"include_left":tc.included}, true)
		if err != nil || out.Value != tc.want {
			t.Fatalf("include_left=%t got %#v, %v", tc.included, out, err)
		}
	}
}

func TestOptionalDoesNotHideMissingStructuredMCPResponse(t *testing.T) {
	src := structuredPortPipeline()
	src = strings.Replace(src, `    mock_structured = {code = "C123"}`, "", 1)
	src = strings.Replace(src, `      customer = {
        type = "object"
        required = ["code"]
        properties = {code = {type = "string"}}
      }`, `      customer = {
        optional = true
        schema = {type = ["object", "null"]}
      }`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	got, err := New(nil).Run(context.Background(), "rev", cfg, "structured", "lookup",
		map[string]any{"id":"C123"}, true)
	if err == nil || !strings.Contains(err.Error(), "missing structured MCP output") {
		t.Fatalf("optional port hid completed MCP response failure: %#v %v", got, err)
	}
	if got.Value != nil { t.Fatal("failed structured source published a result") }
}
