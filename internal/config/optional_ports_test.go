package config

import (
	"strings"
	"testing"
)

func optionalBranchPortSource() string {
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

func TestExplicitOptionalDataPortCompilesAndTracksPresence(t *testing.T) {
	cfg, err := Decode(optionalBranchPortSource())
	if err != nil { t.Fatal(err) }
	plan := cfg.Plans["branches"].Nodes["merge"]
	if !plan.OptionalPorts["left"] || plan.InputPorts["left"] == nil {
		t.Fatalf("optional contract not retained: %+v", plan)
	}
	if plan.Expressions["value"] == nil {
		t.Fatal("port_present expression was not compiled")
	}
}

func TestOptionalPortsRejectUnsafeDecls(t *testing.T) {
	base := optionalBranchPortSource()
	port := `      left = {
        optional = true
        schema = {type = ["string", "null"]}
      }`
	leftEdge := `  edge {
    from = "left"
    to = "merge"
  }`
	cases := []struct{name, src, want string}{
		{"required by default", strings.Replace(base, port, `      left = {type = ["string", "null"]}`, 1), "not guaranteed"},
		{"nonnullable optional", strings.Replace(base, `schema = {type = ["string", "null"]}`, `schema = {type = "string"}`, 1), "optional schema must accept null"},
		{"missing schema", strings.Replace(base, port, `      left = {optional = true}`, 1), "optional port requires"},
		{"false optional flag", strings.Replace(base, `optional = true`, `optional = false`, 1), "optional port requires"},
		{"unexpected wrapper field", strings.Replace(base, `optional = true`, `optional = true
        bypass = true`, 1), "optional port requires"},
		{"producer not control ancestor", strings.Replace(base, leftEdge, "", 1), "not a control predecessor"},
		{"direct unsafe node reference", strings.Replace(base, `value = "port_present.left ? ports.left : 'fallback'"`, `value = "nodes.left.value"`, 1), "not guaranteed to complete"},
		{"unknown presence port", strings.Replace(base, `port_present.left`, `port_present.missing`, 1), "unknown input port missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.src); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
