package config

import (
	"strings"
	"testing"
)

func nestedSuspendConfig() string {
	src := effectfulChildFixture()
	return strings.Replace(src, `  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }`, `  node "create" {
    kind = "suspend"
    output_schema = {type = "string"}
  }`, 1)
}

func TestNestedSuspendRequiresExplicitEffectfulOptIn(t *testing.T) {
	base := nestedSuspendConfig()
	allowed := strings.Replace(base, "    allow_effects = true",
		"    allow_effects = true\n    allow_suspend = true", 1)
	if _, err := Decode(allowed); err != nil {
		t.Fatalf("explicit suspended child rejected: %v", err)
	}
	if _, err := Decode(base); err == nil ||
		!strings.Contains(err.Error(), "allow_suspend=true") {
		t.Fatalf("missing suspension opt-in accepted: %v", err)
	}
	withoutEffects := strings.Replace(allowed, "    allow_effects = true\n", "", 1)
	if _, err := Decode(withoutEffects); err == nil ||
		!strings.Contains(err.Error(), "allow_suspend requires allow_effects") {
		t.Fatalf("pure caller suspended: %v", err)
	}
}

func TestNestedSuspendNeedsEveryEnclosingOptIn(t *testing.T) {
	src := nestedSuspendConfig()
	middle := `pipeline "middle" {
  entry = "entry"
  node "entry" { kind = "input" }
  node "child" {
    kind = "pipeline.call"
    pipeline = "writer"
    arguments = "nodes.entry.value"
    allow_effects = true
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
`
	src = strings.Replace(src, `pipeline "outer" {`, middle+`pipeline "outer" {`, 1)
	src = strings.Replace(src, `pipeline = "writer"
    allow_effects = true
    arguments = "input"`, `pipeline = "middle"
    allow_effects = true
    allow_suspend = true
    arguments = "input"`, 1)
	if _, err := Decode(src); err == nil || !strings.Contains(err.Error(), "allow_suspend=true") {
		t.Fatalf("unapproved grandchild suspension accepted: %v", err)
	}
	allowed := strings.Replace(src, `pipeline = "writer"
    arguments = "nodes.entry.value"
    allow_effects = true`, `pipeline = "writer"
    arguments = "nodes.entry.value"
    allow_effects = true
    allow_suspend = true`, 1)
	if _, err := Decode(allowed); err != nil {
		t.Fatalf("fully opted-in chain rejected: %v", err)
	}
}
