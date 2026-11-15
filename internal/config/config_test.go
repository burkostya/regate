package config

import (
	"os"
	"strings"
	"testing"
)

func example(t *testing.T) string {
	t.Helper()
	b, e := os.ReadFile("../../examples/regate.hcl")
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestExampleAndGraph(t *testing.T) {
	c, e := Decode(example(t))
	if e != nil {
		t.Fatal(e)
	}
	if len(c.Upstreams) != 2 || len(c.Hooks) != 3 || len(c.Pipelines) != 1 || len(c.Surfaces) != 1 {
		t.Fatalf("missing config parts: %+v", c)
	}
	if len(c.Pipelines["customer_summary"].Edges) != 4 {
		t.Fatal("missing edges")
	}
}
func TestRejectCycle(t *testing.T) {
	source := example(t) + "\n" + `pipeline "bad" {
  entry = "a"
  node "a" {
    kind = "transform"
    value = "input"
  }
  node "b" {
    kind = "output"
    value = "input"
  }
  edge {
    from = "a"
    to = "b"
  }
  edge {
    from = "b"
    to = "a"
  }
}`
	if _, e := Decode(source); e == nil || !strings.Contains(e.Error(), "cycle") {
		t.Fatalf("expected cycle error; got %v", e)
	}
}
func TestRejectUnknownAndDuplicate(t *testing.T) {
	for _, src := range []string{
		`fish "x" {a="b"}`, `pipeline "x" {
 entry="x"
 node "x" {
 kind = "output"
 kind = "input"
 }
 }`, `surface "a" {
path="/mcp/a"
tool "b" {source="fake.missing"}
}`,
	} {
		if _, e := Decode(src); e == nil {
			t.Errorf("source accepted unexpectedly: %s", src)
		}
	}
}
func TestPatchPreservesOutsideAndComments(t *testing.T) {
	original := example(t)
	patch := `pipeline "customer_summary" {
  entry = "end"
  node "end" {
    kind = "output"
    value = "input"
  }
}`
	next, e := ReplaceBlock(original, "pipeline", "customer_summary", patch)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(next, "# Regate example") {
		t.Fatal("comments outside block lost")
	}
	if !strings.Contains(next, `hook "summarizer"`) || !strings.Contains(next, `surface "support"`) {
		t.Fatal("unrelated blocks changed")
	}
	if _, e := Decode(next); e != nil {
		t.Fatal(e)
	}
}
func TestUnsupportedExpressionIsError(t *testing.T) {
	if _, e := Parse(`regate { listen = join("a", "b") }`); e == nil {
		t.Fatal("must reject unsupported function")
	}
}

func TestHCL2EvaluationAndStrictTyping(t *testing.T) {
	source := strings.Replace(example(t), "timeout_ms = 5000", "timeout_ms = 2000 * 2", 1)
	cfg, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstreams["crm"].TimeoutMS != 4000 {
		t.Fatal("HCL2 arithmetic not evaluated")
	}
	bad := []struct{ source, word string }{
		{strings.Replace(example(t), `timeout_ms = 5000`, `timeout_ms = "five"`, 1), "timeout_ms"},
		{strings.Replace(example(t), `timeout_ms = 5000`, `timeout_ms = 0`, 1), "timeout_ms"},
		{strings.Replace(example(t), `  timeout_ms = 5000`, `  timeout_mss = 5000`, 1), "timeout_mss"},
		{strings.Replace(example(t), `kind   = "policy.check"`, `kind   = "policy.check"\n    unknown_attr = true`, 1), "unknown_attr"},
		{strings.Replace(example(t), `value  = "input"`, `value  = "1 + true"`, 1), "CEL"},
	}
	for _, tc := range bad {
		_, err := Decode(tc.source)
		if err == nil || !strings.Contains(err.Error(), tc.word) {
			t.Errorf("wanted %q error, got %v", tc.word, err)
		}
	}
}

func TestCELPreflightRejectsTypeAndBadDependencies(t *testing.T) {
	tests := []struct{ find, replace, contains string }{
		{`value = "nodes.mask.value"`, `value = "nodes.absent.value"`, "missing node"},
		{`value = "nodes.mask.value"`, `value = "nodes.finish.value"`, "not a control predecessor"},
		{`value = "nodes.mask.value"`, `value = "nodes.mask.value + true"`, "CEL"},
	}
	for _, tc := range tests {
		source := strings.Replace(example(t), tc.find, tc.replace, 1)
		if source == example(t) {
			t.Fatalf("unmatched source %q", tc.find)
		}
		_, err := Decode(source)
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Errorf("wanted %q, got %v", tc.contains, err)
		}
	}
}

func TestSourcePreservingAttributePatch(t *testing.T) {
	source := example(t)
	path := []BlockSelector{{Type: "pipeline", Label: "customer_summary"}, {Type: "node", Label: "fetch"}}
	next, err := SetAttribute(source, path, "tool", `"get_customer_public"`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Replace(next, `tool      = "get_customer_public"`, `tool      = "get_customer"`, 1) != source {
		t.Fatal("other source bytes changed")
	}
	if _, err := Decode(next); err != nil {
		t.Fatal(err)
	}
	if _, err := SetAttribute(source, path, "upstrem", `"crm"`); err == nil {
		t.Fatal("missing attr should reject")
	}
	if _, err := SetAttribute(source, path, "tool", `"broken`); err == nil {
		t.Fatal("broken expression should reject")
	}
}

func TestSemanticDiffIgnoresWhitespaceAndComments(t *testing.T) {
	original := example(t)
	updated := strings.Replace(original, `tool      = "get_customer"`, `tool  =    "get_customer"`, 1)
	updated = "# A comment that should not affect behavior\n" + updated
	a, err := Decode(original)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Decode(updated)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := SemanticDiff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 0 {
		t.Fatalf("format-only diff should be empty: %+v", diff)
	}
	modified := strings.Replace(original, `tool      = "get_customer"`, `tool      = "get_customer_public"`, 1)
	b, err = Decode(modified)
	if err != nil {
		t.Fatal(err)
	}
	diff, err = SemanticDiff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 1 || !strings.Contains(diff[0].Path, "tool") {
		t.Fatalf("expected one semantic edit, got %+v", diff)
	}
}
