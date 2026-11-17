package engine

import (
    "context"
    "reflect"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
)

// A three-level composition used to spawn up to 3*3 CEL workers, and
// much more when operators raised each limit to 16. The inner declared
// parallelism is still valid HCL but is executed by the outer worker.
func nestedPureMapSource() string {
    return `pipeline "leaf" {
  entry = "start"
  node "start" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["names"]
      properties = {names = {type = "array", items = {type = "string"}}}
    }
  }
  node "upper" {
    kind = "map"
    items = "nodes.start.value.names"
    mapping = "upper(input)"
    max_items = 3
    parallelism = 3
    input_schema = {type = "string"}
    output_schema = {type = "array", items = {type = "string"}}
  }
  node "done" {
    kind = "output"
    value = "nodes.upper.value"
    output_schema = {type = "array", items = {type = "string"}}
  }
  edge {from = "start", to = "upper"}
  edge {from = "upper", to = "done"}
}
pipeline "middle" {
  entry = "start"
  node "start" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["groups"]
      properties = {groups = {type = "array", items = {type = "object"}}}
    }
  }
  node "maps" {
    kind = "map"
    items = "nodes.start.value.groups"
    pipeline = "leaf"
    max_items = 3
    parallelism = 3
    input_schema = {type = "object"}
    output_schema = {type = "array", items = {type = "array", items = {type = "string"}}}
  }
  node "done" {
    kind = "output"
    value = "nodes.maps.value"
    output_schema = {type = "array", items = {type = "array", items = {type = "string"}}}
  }
  edge {from = "start", to = "maps"}
  edge {from = "maps", to = "done"}
}
pipeline "root" {
  entry = "each"
  node "each" {
    kind = "map"
    items = "input.batches"
    pipeline = "middle"
    max_items = 3
    parallelism = 3
    input_schema = {type = "object"}
    output_schema = {
      type = "array"
      items = {type = "array", items = {type = "array", items = {type = "string"}}}
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.each.value"
  }
  edge {from = "each", to = "done"}
}
surface "nested" {
  path = "/mcp/nested"
  tool "run" {
    pipeline = "root"
    read_only = true
  }
}
`
}

func TestNestedPureMapWorkerContextDoesNotMultiplyFanout(t *testing.T) {
    ctx:=context.Background()
    if got:=effectivePureMapParallelism(ctx,16);got!=16 {
        t.Fatalf("top-level parallel map unexpectedly restricted: %d",got)
    }
    workerCtx:=withPureMapWorker(ctx)
    if !inPureMapWorker(workerCtx) {
        t.Fatal("pure-map worker marker was not propagated")
    }
    if got:=effectivePureMapParallelism(workerCtx,16);got!=1 {
        t.Fatalf("nested parallel pool would multiply workers: %d",got)
    }
    if got:=effectivePureMapParallelism(workerCtx,1);got!=1 {
        t.Fatalf("serial map changed: %d",got)
    }
    if withPureMapWorker(workerCtx)!=workerCtx {
        t.Fatal("nested worker stacked unnecessary context wrappers")
    }
}

func TestNestedParallelPipelineAndCELMapPreserveOrderedResults(t *testing.T) {
    cfg,err:=config.Decode(nestedPureMapSource())
    if err!=nil {t.Fatalf("nested pure HCL rejected: %v",err)}
    args:=map[string]any{"batches":[]any{
        map[string]any{"groups":[]any{
            map[string]any{"names":[]any{"c","d"}},
            map[string]any{"names":[]any{"a"}},
        }},
        map[string]any{"groups":[]any{
            map[string]any{"names":[]any{"z","q","p"}},
        }},
    }}
    expected:=[]any{
        []any{[]any{"C","D"},[]any{"A"}},
        []any{[]any{"Z","Q","P"}},
    }
    for i:=0;i<5;i++ {
        got,err:=New(nil).Run(context.Background(),"rev",cfg,"nested","run",args,true)
        if err!=nil || got.Status!="success" || !reflect.DeepEqual(got.Value,expected) {
            t.Fatalf("nested pure map %d reordered or failed: %#v %v",i,got,err)
        }
    }
    child:=cfg.Pipelines["middle"]
    parentPlan:=cfg.Plans["root"].Nodes["each"]
    _,err=New(nil).evaluatePurePipelineMap(withPureMapWorker(context.Background()),cfg,child,
        parentPlan,[]any{args["batches"].([]any)[0]},3,"run","nested","run","rev",true)
    if err==nil || !strings.Contains(err.Error(),"nested pure pipeline map fanout") {
        t.Fatalf("low-level nested worker bypassed the runtime bound: %v",err)
    }
}

func TestNestedPureMapCancellationRemainsFailClosed(t *testing.T) {
    cfg,err:=config.Decode(nestedPureMapSource())
    if err!=nil {t.Fatal(err)}
    cancelled,cancel:=context.WithCancel(context.Background())
    cancel()
    args:=map[string]any{"batches":[]any{map[string]any{"groups":[]any{
        map[string]any{"names":[]any{"a"}},
    }}}}
    got,err:=New(nil).Run(cancelled,"rev",cfg,"nested","run",args,true)
    if err==nil || got.Value!=nil || got.MCP!=nil {
        t.Fatalf("cancelled nested pure map published a partial result: %#v %v",got,err)
    }
}
