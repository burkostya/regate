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
	source := example(t) + "\n" + `pipeline "bad" { entry = "a" node "a" { kind = "transform" value = "input" } node "b" { kind = "output" value = "input" } edge {from = "a" to = "b"} edge {from = "b" to = "a"} }`
	if _, e := Decode(source); e == nil || !strings.Contains(e.Error(), "cycle") {
		t.Fatalf("expected cycle error; got %v", e)
	}
}
func TestRejectUnknownAndDuplicate(t *testing.T) {
	for _, src := range []string{
		`fish "x" {a="b"}`, `pipeline "x" { entry="x" node "x" {kind = "output" kind = "input"}}`, `surface "a" {path="/mcp/a" tool "b" {source="fake.missing"}}`,
	} {
		if _, e := Decode(src); e == nil {
			t.Errorf("source accepted unexpectedly: %s", src)
		}
	}
}
func TestPatchPreservesOutsideAndComments(t *testing.T) {
	original := example(t)
	patch := `pipeline "customer_summary" { entry = "end" node "end" {kind="output" value="input"} }`
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
