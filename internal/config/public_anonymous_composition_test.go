package config

import (
    "os"
    "strings"
    "testing"
)

// This deterministic compiler gate runs even when the external service smoke
// test is skipped. Both real-provider reads must be explicitly designated
// safe, the fan-in must require BOTH results and the exposed tool must not
// misrepresent the composed operation as writable.
func TestPublicAnonymousTwoProviderDAGContracts(t *testing.T) {
    data,err:=os.ReadFile("../../examples/public-anonymous-mcp.hcl")
    if err!=nil {t.Fatal(err)}
    source:=string(data)
    cfg,err:=Decode(source)
    if err!=nil {t.Fatalf("public composed provider HCL failed compilation: %v",err)}
    pipeline,ok:=cfg.Pipelines["public_documentation_probe"]
    if !ok {t.Fatal("missing composed provider pipeline")}
    if pipeline.Concurrency!=2 || pipeline.Nodes["both"].Str("mode")!="all" {
        t.Fatal("real third-party providers must join at the full two-read barrier")
    }
    for node,upstream:=range map[string]string{"docs":"mslearn","wiki":"deepwiki"} {
        read,exists:=pipeline.Nodes[node]
        if !exists || read.Kind!="mcp.call" || read.Str("upstream")!=upstream ||
            read.Str("effect")!="read" {
            t.Fatalf("not an explicitly read-only independent upstream call: %s %#v",node,read)
        }
        plan:=cfg.Plans[pipeline.Name]
        if plan==nil || plan.Nodes[node]==nil || plan.Nodes[node].OutputPorts["content"]==nil {
            t.Fatalf("missing typed actual vendor content port on %s",node)
        }
    }
    published,ok:=cfg.Surfaces["combined"]
    if !ok || len(published.Tools)!=1 || published.Tools[0].Name!="public_docs_check" ||
        !published.Tools[0].ReadOnly || published.Tools[0].Pipeline!=pipeline.Name {
        t.Fatalf("combined public surface contract is not read-only and isolated: %#v",published)
    }

    // With an all-join every consumer can refer to both read outputs.
    // Weakening the join would allow one branch's values to be absent; the
    // compiler must refuse such a non-guaranteed data dependency.
    weakened:=strings.Replace(source,`mode = "all"`,`mode = "any"`,1)
    if weakened==source {t.Fatal("fixture join anchor changed")}
    if _,err:=Decode(weakened);err==nil {
        t.Fatal("any-join unexpectedly guarantees both unconditionally read provider values")
    }
    malformed:=strings.Replace(source,
        `content = {type = "array", minItems = 1}`,
        `content = {type = "object"}`,1)
    if malformed==source {t.Fatal("fixture typed content port anchor changed")}
    if _,err:=Decode(malformed);err==nil {
        t.Fatal("MCP content output port accepted a non-array schema")
    }
}
