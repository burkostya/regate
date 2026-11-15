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
		{strings.Replace(example(t), `kind   = "policy.check"`, "kind   = \"policy.check\"\n    unknown_attr = true", 1), "unknown_attr"},
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

func TestGraphPatchPreservesNestedCommentsWhenOnlyAttributesChange(t *testing.T) {
	source := strings.Replace(example(t), `    tool      = "get_customer"`, `    # node-specific operator note
    tool      = "get_customer"`, 1)
	blocks, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	var original string
	for _, b := range blocks {
		if b.Type == "pipeline" && len(b.Labels) == 1 && b.Labels[0] == "customer_summary" {
			original = source[b.Start:b.End]
		}
	}
	if original == "" {
		t.Fatal("pipeline not found")
	}
	modified := strings.Replace(original, `tool      = "get_customer"`, `tool      = "get_customer_public"`, 1)
	next, err := ReplaceBlock(source, "pipeline", "customer_summary", modified)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next, "# node-specific operator note") {
		t.Fatal("original nested comment was lost")
	}
	if strings.Replace(next, `tool      = "get_customer_public"`, `tool      = "get_customer"`, 1) != source {
		t.Fatal("graph patch rewrote unrelated source bytes")
	}
	if _, err := Decode(next); err != nil {
		t.Fatal(err)
	}
}

func TestStructuralPatchPreservesOpaqueNestedText(t *testing.T) {
	source := strings.Replace(example(t), `    tool      = "get_customer"`, `    # kept only in the original nested block
    tool      = "get_customer"`, 1)
	original := mustPipelineSource(t, source, "customer_summary")
	// The replacement comes from a visual editor that knows nothing about the
	// operator comment, and restructures the graph by adding a node and an edge.
	desired := strings.Replace(original, `  node "summarize" {`, `  node "audit" {
    kind = "transform"
    value = "nodes.mask.value"
  }

  node "summarize" {`, 1)
	desired = strings.Replace(desired, `from = "mask"
    to   = "summarize"`, `from = "mask"
    to   = "audit"`, 1)
	desired = strings.Replace(desired, `  edge {
    from = "summarize"`, `  edge {
    from = "audit"
    to   = "summarize"
  }
  edge {
    from = "summarize"`, 1)
	updated, err := ReplaceBlock(source, "pipeline", "customer_summary", desired)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(updated, "# kept only in the original nested block") {
		t.Fatal("lost original nested comment")
	}
	if !strings.Contains(updated, `node "audit"`) {
		t.Fatal("missing added node")
	}
	if !strings.Contains(updated, `to   = "audit"`) {
		t.Fatal("missing changed route")
	}
	if _, err = Decode(updated); err != nil {
		t.Fatal(err)
	}
	if strings.Replace(updated, `    # kept only in the original nested block\n`, "", 1) == source {
		t.Fatal("expected the graph to change")
	}
	restored, err := ReplaceBlock(updated, "pipeline", "customer_summary", original)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restored, "# kept only in the original nested block") {
		t.Fatal("structural removal lost preserved comment")
	}
	if strings.Contains(restored, `node "audit"`) {
		t.Fatal("failed to remove new node")
	}
	if _, err = Decode(restored); err != nil {
		t.Fatal(err)
	}
}
func mustPipelineSource(t *testing.T, source, name string) string {
	t.Helper()
	blocks, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blocks {
		if b.Type == "pipeline" && len(b.Labels) == 1 && b.Labels[0] == name {
			return source[b.Start:b.End]
		}
	}
	t.Fatal("missing pipeline")
	return ""
}
func TestSourcePreservingAttributeInsertion(t *testing.T) {
	source := strings.Replace(example(t), `    tool      = "get_customer"`, `    # operator note
    tool      = "get_customer"`, 1)
	path := []BlockSelector{{Type: "pipeline", Label: "customer_summary"}, {Type: "node", Label: "fetch"}}
	next, err := AddAttribute(source, path, "input_schema", `{type="object",required=["customer_id"],properties={customer_id={type="string"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next, "# operator note") {
		t.Fatal("operator note lost")
	}
	if _, err = Decode(next); err != nil {
		t.Fatal(err)
	}
	if _, err = AddAttribute(next, path, "input_schema", `{type="object"}`); err == nil {
		t.Fatal("duplicate accepted")
	}
	if _, err = AddAttribute(source, path, "invalid_name", `1+`); err == nil {
		t.Fatal("invalid expression accepted")
	}
}
func TestSchemaPreflightRejectsProvablePortMismatch(t *testing.T) {
	src := example(t)
	src = strings.Replace(src, `    mock      = {customer_id = "C123", name = "Alice", email = "alice@example.com"}`, `    mock      = {customer_id = "C123", name = "Alice", email = "alice@example.com"}
    output_schema = {type="object",required=["customer_id"],properties={customer_id={type="string"}, email={type="string"}}}`, 1)
	src = strings.Replace(src, `    hook  = "mask_pii"`, `    hook  = "mask_pii"
    input_schema = {type="string"}`, 1)
	if _, err := Decode(src); err == nil || !strings.Contains(err.Error(), "incompatible port types") {
		t.Fatalf("expected static type mismatch, got %v", err)
	}
}
func TestJSONSchemaFullDraftAndOfflineRefs(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"$defs":                map[string]any{"identifier": map[string]any{"type": "string", "pattern": "^[A-Z]{2}[0-9]+$"}},
		"properties":           map[string]any{"id": map[string]any{"$ref": "#/$defs/identifier"}},
		"required":             []any{"id"},
		"additionalProperties": false,
	}
	compiled, err := compileSchema(schema)
	if err != nil {
		t.Fatal(err)
	}
	if err = compiled.Validate(map[string]any{"id": "AB12"}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{map[string]any{"id": "bad"}, map[string]any{"id": "AB12", "extra": true}, map[string]any{}} {
		if err = compiled.Validate(v); err == nil {
			t.Errorf("invalid value accepted: %#v", v)
		}
	}
	if _, err = compileSchema(map[string]any{"$ref": "https://example.org/schema.json"}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("remote schema ref not rejected: %v", err)
	}
}
func TestDynamicRefinementOnUnknownPort(t *testing.T) {
	src := strings.Replace(example(t), `    hook  = "mask_pii"`, `    hook  = "mask_pii"
    input_schema = {type="object",properties={email={type="string"}},required=["email"]}`, 1)
	cfg, err := Decode(src)
	if err != nil {
		t.Fatalf("dynamic connection should compile for runtime checking: %v", err)
	}
	compiled := cfg.Plans["customer_summary"].Nodes["mask"].InputSchema
	if compiled == nil {
		t.Fatal("missing runtime schema")
	}
	if err = compiled.Validate(map[string]any{"email": 3}); err == nil {
		t.Fatal("failed to reject mismatched dynamic result")
	}
}
