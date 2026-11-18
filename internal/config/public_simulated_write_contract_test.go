package config

import (
    "os"
    "strings"
    "testing"
)

// The simulated public write acceptance fixture must never turn an untrusted
// MCP tool argument into an external record. Compile it in normal offline CI
// even when the opt-in live-network acceptance is skipped.
func TestPublicSimulatedWriteFixtureContracts(t *testing.T) {
    raw,err:=os.ReadFile("../../examples/public-simulated-write-mcp.hcl")
    if err!=nil {t.Fatal(err)}
    source:=string(raw)
    cfg,err:=Decode(source)
    if err!=nil {t.Fatalf("synthetic public write HCL invalid: %v",err)}
    if len(cfg.Upstreams)!=1 || cfg.Upstreams["sondahub"].URL!="https://api.sondahub.com/mcp" {
        t.Fatal("test-only external target is not pinned to Sondahub")
    }
    for _,name:=range []string{"sandbox_create","sandbox_rejected"} {
        pipeline,ok:=cfg.Pipelines[name]
        if !ok {t.Fatalf("missing synthetic pipeline %s",name)}
        entry,exists:=pipeline.Nodes["entry"]
        if !exists || entry.Kind!="input" {t.Fatalf("%s has no input contract",name)}
        input:=cfg.Plans[name].Nodes["entry"].InputSchema
        if input==nil || input.Raw["additionalProperties"]!=false {
            t.Fatalf("%s input can introduce unexpected request fields",name)
        }
        write,ok:=pipeline.Nodes["write"]
        if !ok || write.Kind!="mcp.call" || write.Str("upstream")!="sondahub" ||
            write.Str("tool")!="create_record" || write.Str("effect")!="write" {
            t.Fatalf("%s has an untracked or unexpected external effect",name)
        }
        args,ok:=write.Attrs["arguments"].(map[string]any)
        if !ok || args["api"]!="literal:store" || args["collection"]!="literal:categories" {
            t.Fatalf("%s synthetic target is not static",name)
        }
        record,ok:=args["record"].(map[string]any)
        if !ok || len(record)<1 {t.Fatalf("%s missing static synthetic record",name)}
        for field,value:=range record {
            text,ok:=value.(string)
            if !ok || !strings.HasPrefix(text,"literal:") ||
                strings.Contains(text,"input.") {
                t.Fatalf("%s external record field %q is not immutable literal",name,field)
            }
        }
    }
    if len(cfg.Surfaces["sandbox"].Tools)!=2 {
        t.Fatal("public synthetic surface exposes unexpected extra tools")
    }
    for _,tool:=range cfg.Surfaces["sandbox"].Tools {
        if tool.ReadOnly || tool.Pipeline=="" {
            t.Fatalf("simulated effect disguised as a read-only tool: %#v",tool)
        }
    }
}
