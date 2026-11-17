package config

import (
	"strings"
	"testing"
)

func effectfulChildFixture() string {
	return `upstream "crm" {
  url = "https://api.example.com/mcp"
}
pipeline "writer" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.create.value"
  }
  edge {
    from = "entry"
    to = "create"
  }
  edge {
    from = "create"
    to = "done"
  }
}
pipeline "outer" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    arguments = "input"
  }
  node "done" {
    kind = "output"
    value = "nodes.call.value"
  }
  edge {
    from = "call"
    to = "done"
  }
}
surface "nested" {
  path = "/mcp/nested"
  tool "write" {
    pipeline = "outer"
    read_only = false
  }
}
`
}

func TestEffectfulPipelineCallRequiresExplicitOptIn(t *testing.T) {
	base := effectfulChildFixture()
	if _, err := Decode(base); err != nil { t.Fatal(err) }
	without := strings.Replace(base, `    allow_effects = true`, "", 1)
	if _, err := Decode(without); err == nil ||
		!strings.Contains(err.Error(), "cannot execute inside a pure pipeline.call") {
		t.Fatalf("unapproved effectful child accepted: %v", err)
	}
}

func TestEffectfulPipelineCallRejectsNestedSuspension(t *testing.T) {
	base := effectfulChildFixture()
	call := `  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }`
	childSuspend := `  node "create" {
    kind = "suspend"
  }`
	src := strings.Replace(base, call, childSuspend, 1)
	if _, err := Decode(src); err == nil ||
		!strings.Contains(err.Error(), "suspend requires durable nested checkpoint support") {
		t.Fatalf("nested suspend accepted: %v", err)
	}
}

func TestPureNestedCallCannotDelegateEffectsTransitively(t *testing.T) {
	base := effectfulChildFixture()
	// Root -> mid defaults to pure. Mid cannot opt back in to effects
	// that were not admitted at the root's invocation boundary.
	source := strings.Replace(base, `pipeline "outer" {`, `pipeline "mid" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    arguments = "nodes.entry.value"
    allow_effects = true
  }
  node "done" {
    kind = "output"
    value = "nodes.call.value"
  }
  edge {
    from = "entry"
    to = "call"
  }
  edge {
    from = "call"
    to = "done"
  }
}
pipeline "outer" {`, 1)
	source = strings.Replace(source, `    pipeline = "writer"
    allow_effects = true
    arguments = "input"`, `    pipeline = "mid"
    arguments = "input"`, 1)
	if _, err := Decode(source); err == nil || !strings.Contains(err.Error(), "cannot delegate effects") {
		t.Fatalf("pure parent delegated write: %v", err)
	}
}

func TestMandatoryDecisionDominatesEffectfulPipelineCall(t *testing.T) {
	base := effectfulChildFixture()
	const outer = `pipeline "outer" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    arguments = "input"
  }
  node "done" {
    kind = "output"
    value = "nodes.call.value"
  }
  edge {
    from = "call"
    to = "done"
  }
}`
	const secure = `pipeline "outer" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "authorize" {
    kind = "policy.check"
    policy = "gate"
    mandatory = true
    value = "input"
  }
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    arguments = "input"
  }
  node "done" {
    kind = "output"
    value = "nodes.call.value"
  }
  edge {
    from = "start"
    to = "authorize"
  }
  edge {
    from = "authorize"
    to = "call"
  }
  edge {
    from = "call"
    to = "done"
  }
}`
	if !strings.Contains(base, outer) {t.Fatal("outer fixture changed")}
	prefix := `hook "decision" {
  transport = "http"
  url = "https://auth.example.com/decision"
  contract = "decision"
}
policy "gate" {
  decision_hook = "decision"
}
`
	authorized := prefix + strings.Replace(base, outer, secure, 1)
	if _, err := Decode(authorized); err != nil { t.Fatalf("valid authorization path rejected: %v",err) }
	const safeEdge = `  edge {
    from = "authorize"
    to = "call"
  }`
	bypass := strings.Replace(authorized, safeEdge, `  edge {
    from = "start"
    to = "call"
  }
`+safeEdge, 1)
	if _,err:=Decode(bypass);err==nil ||
		!strings.Contains(err.Error(), "mandatory policy authorize") ||
		!strings.Contains(err.Error(), "call") {
		t.Fatalf("effectful child bypassed authorization: %v",err)
	}
}
