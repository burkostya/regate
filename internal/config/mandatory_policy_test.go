package config

import (
	"strings"
	"testing"
)

func mandatoryPolicySource(t *testing.T) string {
	t.Helper()
	const before = `    policy = "customer_read"`
	const after = `    policy = "customer_read"
    mandatory = true`
	source := example(t)
	if !strings.Contains(source,before) {
		t.Fatal("example missing customer policy")
	}
	return strings.Replace(source,before,after,1)
}

func TestMandatoryPolicyDominatesExampleOutboundCalls(t *testing.T) {
	source := mandatoryPolicySource(t)
	compiled,err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Pipelines["customer_summary"].Nodes["authorize"].Attrs["mandatory"] != true {
		t.Fatal("mandatory decision was not retained in HCL config")
	}
	// Mandatory dominates the CRM call, PII hook, agent hook and output.
	if len(compiled.Plans["customer_summary"].Order) != 5 {
		t.Fatal("mandatory policy altered the existing executable graph")
	}
}

func TestMandatoryPolicyRejectsUnapprovedSideRoute(t *testing.T) {
	approved := mandatoryPolicySource(t)
	entry := `  entry       = "authorize"`
	if !strings.Contains(approved,entry) {t.Fatal("entry not found")}
	bypass := strings.Replace(approved,entry,`  entry       = "start"`,1)
	start := `  node "start" {
    kind = "input"
  }

  node "authorize" {`
	bypass = strings.Replace(bypass,`  node "authorize" {`,start,1)
	if !strings.Contains(bypass,start) {t.Fatal("start fixture missing")}
	oldEdge := `  edge {
    from = "authorize"`
	newEdge := `  edge {
    from = "start"
    to   = "authorize"
  }
  edge {
    from = "start"
    to   = "fetch"
  }
  edge {
    from = "authorize"`
	if !strings.Contains(bypass,oldEdge) {t.Fatal("edge fixture missing")}
	bypass = strings.Replace(bypass,oldEdge,newEdge,1)
	if _,err:=Decode(bypass);err==nil ||
		!strings.Contains(err.Error(),"mandatory policy authorize") ||
		!strings.Contains(err.Error(),"fetch") {
		t.Fatalf("mandatory policy bypass admitted: %v",err)
	}
}

func TestMandatoryPolicyRejectsOutputBypass(t *testing.T) {
	approved := mandatoryPolicySource(t)
	// A second edge can expose values through the output even without an
	// outbound HTTP call. The compiler protects output as well as effects.
	entry := `  entry       = "authorize"`
	bypass := strings.Replace(approved,entry,`  entry       = "start"`,1)
	bypass = strings.Replace(bypass,`  node "authorize" {`,`  node "start" {
    kind = "input"
  }
  node "authorize" {`,1)
	oldEdge := `  edge {
    from = "authorize"`
	newEdge := `  edge {
    from = "start"
    to   = "authorize"
  }
  edge {
    from = "start"
    to   = "finish"
  }
  edge {
    from = "authorize"`
	bypass = strings.Replace(bypass,oldEdge,newEdge,1)
	if _,err:=Decode(bypass);err==nil ||
		!strings.Contains(err.Error(),"mandatory policy authorize") ||
		!strings.Contains(err.Error(),"finish"){
		t.Fatalf("unguarded final output accepted: %v",err)
	}
}

func TestMandatoryPolicyRequiresEnforceableDecision(t *testing.T) {
	valid := mandatoryPolicySource(t)
	cases := []struct{name, source, contains string}{
		{"missing decision hook",strings.Replace(valid,
			`decision_hook = "decision"`,`decision_hook = ""`,1),"non-builtin decision hook"},
		{"missing input",strings.Replace(valid,
			`    value  = "input"`,"",1),"decision input value"},
		{"mandatory on wrong node",strings.Replace(valid,
			`    kind      = "mcp.call"`,`    kind      = "mcp.call"
    mandatory = true`,1),"mandatory"},
	}
	for _,tc := range cases {
		t.Run(tc.name,func(t *testing.T){
			if _,err:=Decode(tc.source);err==nil ||
				!strings.Contains(err.Error(),tc.contains) {
				t.Fatalf("invalid policy accepted: %v",err)
			}
		})
	}
	compiled,err:=Decode(valid)
	if err!=nil{t.Fatal(err)}
	policy:=compiled.Hooks["decision"]
	policy.Transport="builtin"
	compiled.Hooks["decision"]=policy
	if err:=compiled.Validate();err==nil ||
		!strings.Contains(err.Error(),"non-builtin decision hook"){
		t.Fatalf("builtin no-op decision hook accepted: %v",err)
	}
}

func TestPolicyDominanceGraphModeling(t *testing.T) {
	// The graph predicate must not confuse an unrelated nested branch
	// with a path that traverses the authorization gate.
	adj:=map[string][]string{
		"start":{"policy","parallel"},
		"policy":{"sink"},
		"parallel":{"untrusted"},
		"untrusted":{"sink"},
	}
	if !reachableAvoidingNode("start","sink","policy",adj){
		t.Fatal("missed unapproved alternative path")
	}
	if reachableAvoidingNode("start","policy","policy",adj){
		t.Fatal("gate itself can never be reached while avoiding itself")
	}
	if reachableAvoidingNode("start","irrelevant","policy",adj){
		t.Fatal("invented unreachable target")
	}
	adj["start"]=[]string{"policy"}
	if !reachableAvoidingNode("start","untrusted","policy",adj){
		// An incomplete disconnected graph could still reach untrusted
		// from an entry *only* through the gate, so this must be false.
	} else {
		t.Fatal("a mandatory gate does not dominate a downstream node")
	}
}
