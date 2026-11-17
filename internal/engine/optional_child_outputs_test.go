package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func optionalChildOutputsFixture() string {
	return `pipeline "child" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "pick" {
    kind = "condition"
    test = "input.left"
  }
  node "yes" {
    kind = "output"
    value = "literal:LEFT"
    output_schema = {type = "string"}
  }
  node "no" {
    kind = "output"
    value = "literal:RIGHT"
    output_schema = {type = "string"}
  }
  edge {
    from = "start"
    to = "pick"
  }
  edge {
    from = "pick"
    to = "yes"
    when = "true"
  }
  edge {
    from = "pick"
    to = "no"
    when = "false"
  }
}
pipeline "parent" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "child"
    arguments = "input"
    output_ports = {
      yes = {optional = true, schema = {type = "string"}}
      no = {optional = true, schema = {type = "string"}}
    }
  }
  node "done" {
    kind = "output"
    value = "port_present.left ? ports.left : ports.right"
    input_ports = {
      left = {optional = true, schema = {type = ["string", "null"]}}
      right = {optional = true, schema = {type = ["string", "null"]}}
    }
  }
  edge {
    from = "call"
    to = "done"
  }
  edge {
    kind = "data"
    from = "call"
    from_port = "yes"
    to = "done"
    to_port = "left"
  }
  edge {
    kind = "data"
    from = "call"
    from_port = "no"
    to = "done"
    to_port = "right"
  }
}
surface "optional" {
  path = "/mcp/optional"
  tool "choose" {
    pipeline = "parent"
    read_only = true
  }
}
`
}

func TestOptionalConditionalChildOutputsAreDistinctFromNull(t *testing.T) {
	cfg, err := config.Decode(optionalChildOutputsFixture())
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct{left bool; want string}{{true, "LEFT"}, {false, "RIGHT"}} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "choose",
			map[string]any{"left": tc.left}, true)
		if err != nil || out.Value != tc.want {
			t.Fatalf("conditional child output %v: %#v %v", tc.left, out, err)
		}
	}
	// The public default value is the sparse aggregate object: absent
	// branch outputs are not silently converted into present nulls.
	src := strings.Replace(optionalChildOutputsFixture(),
		`value = "port_present.left ? ports.left : ports.right"`,
		`value = "nodes.call.value"`, 1)
	cfg, err = config.Decode(src)
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct { left bool; want map[string]any }{
		{true, map[string]any{"yes": "LEFT"}},
		{false, map[string]any{"no": "RIGHT"}},
	} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "choose",
			map[string]any{"left": tc.left}, true)
		if err != nil || !reflect.DeepEqual(out.Value, tc.want) {
			t.Fatalf("optional aggregate %v: got %#v (%v), want %#v", tc.left, out, err, tc.want)
		}
	}
}

func TestOptionalPresentNullIsNotAnAbsentPort(t *testing.T) {
	src := optionalChildOutputsFixture()
	src = strings.Replace(src, `value = "literal:LEFT"`, `value = "null"`, 1)
	src = strings.Replace(src, `output_schema = {type = "string"}`,
		`output_schema = {type = ["string", "null"]}`, 1)
	src = strings.Replace(src, `yes = {optional = true, schema = {type = "string"}}`,
		`yes = {optional = true, schema = {type = ["string", "null"]}}`, 1)
	src = strings.Replace(src, `value = "port_present.left ? ports.left : ports.right"`,
		`value = "port_present.left"`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct {left, present bool}{{true, true}, {false, false}} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "choose",
			map[string]any{"left": tc.left}, true)
		if err != nil || out.Value != tc.present {
			t.Fatalf("null-vs-absence %v: %#v %v", tc.left, out, err)
		}
	}
}

func TestOptionalChildOutputRejectsUnsafeDeclarations(t *testing.T) {
	base := optionalChildOutputsFixture()
	yes := `yes = {optional = true, schema = {type = "string"}}`
	left := `left = {optional = true, schema = {type = ["string", "null"]}}`
	cases := []struct{name, source, want string}{
		{"optional output without schema", strings.Replace(base, yes, `yes = {optional = true}`, 1), "optional output requires optional=true and schema only"},
		{"optional false", strings.Replace(base, yes, `yes = {optional = false, schema = {type = "string"}}`, 1), "optional output requires optional=true and schema only"},
		{"optional schema not object", strings.Replace(base, yes, `yes = {optional = true, schema = "string"}`, 1), "optional schema must be an object"},
		{"optional value output", strings.Replace(base, yes, yes + `
      value = {optional = true, schema = {type = "object"}}`, 1), "only named pipeline.call outputs may be optional"},
		{"required consumer", strings.Replace(base, left, `left = {type = "string"}`, 1), "optional child output requires optional consumer input port"},
		{"unknown child source", strings.Replace(base, `from_port = "yes"`, `from_port = "unbound"`, 1), "requires declared pipeline.call output port unbound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestOptionalChildOutputRequiredTerminalStillFailsClosed(t *testing.T) {
	src := strings.Replace(optionalChildOutputsFixture(),
		`no = {optional = true, schema = {type = "string"}}`,
		`no = {type = "string"}`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "optional", "choose",
		map[string]any{"left": true}, true)
	if err == nil || !strings.Contains(err.Error(), "missing required child output no") {
		t.Fatalf("required missing terminal accepted: %#v %v", out, err)
	}
	if out.Value != nil || out.OutputProduced {
		t.Fatalf("incomplete child result published: %#v", out)
	}
}

func TestOptionalNamedPropertyOfSingleChildObject(t *testing.T) {
	src := namedCallExecutableSource()
	src = strings.Replace(src,
		`value = {first = "input.first", second = "input.second"}`,
		`value = {first = "input.first"}`, 1)
	src = strings.Replace(src,
		`required = ["first", "second"]`, `required = ["first"]`, 1)
	src = strings.Replace(src,
		`output_ports = {first = {type = "string"}, second = {type = "string"}}`,
		`output_ports = {first = {type = "string"}, second = {optional = true, schema = {type = "string"}}}`, 1)
	src = strings.Replace(src,
		`input_ports = {left = {type = "string"}, right = {type = "string"}}`,
		`input_ports = {left = {type = "string"}, right = {optional = true, schema = {type = ["string", "null"]}}}`, 1)
	src = strings.Replace(src, `value = "ports.left + ':' + ports.right"`,
		`value = "port_present.right ? ports.right : 'absent'"`, 1)
	cfg, err := config.Decode(src)
	if err != nil { t.Fatal(err) }
	out, err := New(nil).Run(context.Background(), "rev", cfg, "named", "combine",
		map[string]any{"first":"Ada"}, true)
	if err != nil || out.Value != "absent" {
		t.Fatalf("optional single-object property not routed: %#v %v", out, err)
	}
}
