package config

import (
	"strings"
	"testing"
)

func multiTerminalMapSource() string {
	src := mappedSubpipelineSource()
	src = strings.Replace(src, `  node "done" {
    kind = "output"
    value = "nodes.start.value"
    output_schema = {type = "object"}
  }`, `  node "left" {
    kind = "output"
    value = "nodes.start.value.left"
    output_schema = {type = "string"}
  }
  node "right" {
    kind = "output"
    value = "nodes.start.value.right"
    output_schema = {type = "string"}
  }`, 1)
	src = strings.Replace(src, `  edge {
    from = "start"
    to = "done"
  }`, `  edge {
    from = "start"
    to = "left"
  }
  edge {
    from = "start"
    to = "right"
  }`, 1)
	src = strings.Replace(src,
		`output_schema = {type = "array", items = {type = "object"}}`,
		`output_schema = {
      type = "array"
      items = {
        type = "object"
        required = ["left"]
        properties = {
          left = {type = "string"}
          right = {type = "string"}
        }
        additionalProperties = false
      }
    }`, 1)
	return src
}

func TestMappedMultiTerminalContractsAreCompiled(t *testing.T) {
	cfg, err := Decode(multiTerminalMapSource())
	if err != nil {
		t.Fatal(err)
	}
	item := cfg.Plans["batch"].Nodes["each"]
	if item.MapItemSchema == nil || item.OutputSchema == nil || item.MapItemSchema.Types()[0] != "object" {
		t.Fatalf("multi-output item schema missing: %#v", item)
	}
}

func TestMappedMultiTerminalRejectsAmbiguousOrUntypedFields(t *testing.T) {
	base := multiTerminalMapSource()
	cases := []struct {
		name, source, want string
	}{
		{"open items", strings.Replace(base, "additionalProperties = false", "additionalProperties = true", 1), "requires closed object items schema"},
		{"missing child field", strings.Replace(base, `          right = {type = "string"}`, "", 1), "exactly the child terminal fields"},
		{"extra child field", strings.Replace(base, `          right = {type = "string"}`, `          right = {type = "string"}
          extra = {type = "string"}`, 1), "exactly the child terminal fields"},
		{"incompatible child field", strings.Replace(base, `          right = {type = "string"}`, `          right = {type = "integer"}`, 1), "incompatible port types"},
		{"no required terminals", strings.Replace(base, `required = ["left"]`, "required = []", 1), "requires at least one required terminal"},
		{"unknown required terminal", strings.Replace(base, `required = ["left"]`, `required = ["unknown"]`, 1), "unknown required mapped child terminal"},
		{"untyped child terminal", strings.Replace(base, `  node "right" {
    kind = "output"
    value = "nodes.start.value.right"
    output_schema = {type = "string"}
  }`, `  node "right" {
    kind = "output"
    value = "nodes.start.value.right"
  }`, 1), "requires output_schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
