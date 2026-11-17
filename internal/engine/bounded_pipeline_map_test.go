package engine

import (
    "context"
    "errors"
    "fmt"
    "reflect"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
)

func boundedPipelineMapSource(url string) string {
    return fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "writer" {
  entry = "start"
  node "start" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["id"]
      properties = {id = {type = "string"}}
    }
  }
  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.start.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.create.value"
    output_schema = {type = "object"}
  }
  edge {
    from = "start"
    to = "create"
  }
  edge {
    from = "create"
    to = "done"
  }
}
pipeline "batch" {
  entry = "iterate"
  node "iterate" {
    kind = "map"
    items = "input.records"
    pipeline = "writer"
    allow_effects = true
    max_items = 3
    input_schema = {type = "object"}
    output_schema = {
      type = "array"
      items = {type = "object"}
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.iterate.value"
  }
  edge {
    from = "iterate"
    to = "done"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "write" {
    pipeline = "batch"
    read_only = false
  }
}
`,url)
}

func TestBoundedMappedSubrunsHaveSeparateDurableWriteIdentities(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,false)
    defer upstream.Close()
    cfg,err:=config.Decode(boundedPipelineMapSource(upstream.URL))
    if err!=nil {t.Fatal(err)}
    store:=effectTestStore(t)
    args:=map[string]any{"records":[]any{
        map[string]any{"id":"A"},map[string]any{"id":"B"},
    }}
    out,err:=New(store).Run(context.Background(),"rev",cfg,"mapped","write",args,false)
    if err!=nil || out.Status!="success" || calls.Load()!=2 {
        t.Fatalf("bounded map failed: %#v %v writes=%d",out,err,calls.Load())
    }
    values,ok:=out.Value.([]any)
    if !ok || len(values)!=2 {
        t.Fatalf("mapped result order/length lost: %#v",out.Value)
    }
    want:=[]any{map[string]any{"created":true},map[string]any{"created":true}}
    if !reflect.DeepEqual(values,want) {
        t.Fatalf("unexpected results: %#v",values)
    }
    records:=store.Effects()
    if len(records)!=2 || records[0].RunID==records[1].RunID ||
        records[0].RunID==out.RunID || records[0].Status!="confirmed" ||
        records[1].Status!="confirmed" {
        t.Fatalf("mapped writes lack unique durable subruns: %#v",records)
    }
    _,err=New(store).execute(withEffectScope(context.Background(),out.RunID),"rev",cfg,
        cfg.Pipelines["batch"],out.RunID,"mapped","write",args,false,nil)
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) || calls.Load()!=2 {
        t.Fatalf("replayed already confirmed map write: %v writes=%d",err,calls.Load())
    }
}

func TestMappedPreflightRejectsInvalidLaterItemBeforeAnyWrite(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,false)
    defer upstream.Close()
    cfg,err:=config.Decode(boundedPipelineMapSource(upstream.URL))
    if err!=nil {t.Fatal(err)}
    for _,items:=range [][]any {
        {map[string]any{"id":"A"},map[string]any{"id":42}},
        {map[string]any{"id":"A"},42},
        {map[string]any{"id":"A"},map[string]any{"id":"B"},map[string]any{"id":"C"},map[string]any{"id":"D"}},
    } {
        out,err:=New(effectTestStore(t)).Run(context.Background(),"rev",cfg,"mapped","write",
            map[string]any{"records":items},false)
        if err==nil || out.Status!="failure" || out.Value!=nil ||
            out.OutputProduced || calls.Load()!=0 {
            t.Fatalf("malformed batch caused effects or public result: %#v %v writes=%d",out,err,calls.Load())
        }
    }
}

func TestUnknownMappedWriteStopsWithoutRetryOrPartialOutput(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,true)
    defer upstream.Close()
    cfg,err:=config.Decode(boundedPipelineMapSource(upstream.URL))
    if err!=nil {t.Fatal(err)}
    store:=effectTestStore(t)
    args:=map[string]any{"records":[]any{map[string]any{"id":"A"},map[string]any{"id":"B"}}}
    out,err:=New(store).Run(context.Background(),"rev",cfg,"mapped","write",args,false)
    if err==nil || out.Value!=nil || out.Status!="failure" || calls.Load()!=1 {
        t.Fatalf("unknown first write was hidden/retried: %#v %v writes=%d",out,err,calls.Load())
    }
    records:=store.Effects()
    if len(records)!=1 || records[0].Status!="unknown" {
        t.Fatalf("unknown map effect not journaled: %#v",records)
    }
    _,err=New(store).execute(withEffectScope(context.Background(),out.RunID),"rev",cfg,
        cfg.Pipelines["batch"],out.RunID,"mapped","write",args,false,nil)
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) || calls.Load()!=1 {
        t.Fatalf("unknown mapped write repeated: %v writes=%d",err,calls.Load())
    }
}

func TestPureCELMapStillWorksAndMappedSuspendCannotActivate(t *testing.T) {
    cfg,err:=config.Decode(boundedMapPipeline())
    if err!=nil {t.Fatal(err)}
    out,err:=New(nil).Run(context.Background(),"rev",cfg,"mapped","names",
        map[string]any{"names":[]any{"alpha"}},true)
    if err!=nil || !reflect.DeepEqual(out.Value,[]any{"ALPHA"}) {
        t.Fatalf("legacy pure map regressed: %#v %v",out,err)
    }

    // Suspension is forbidden under a map, including if its own effect flag
    // is enabled: per-item nested checkpoints are not implemented.
    src:=boundedPipelineMapSource("http://localhost:1234")
    src=strings.Replace(src,`  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.start.value"
  }`,`  node "create" {
    kind = "suspend"
  }`,1)
    if _,err=config.Decode(src);err==nil || !strings.Contains(err.Error(),"suspend requires") {
        t.Fatalf("unsafe mapped suspension accepted: %v",err)
    }
}

func TestMappedPureChildPreservesInputOrderWithoutEffects(t *testing.T) {
    src:=boundedPipelineMapSource("http://localhost:1234")
    src=strings.Replace(src,`  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.start.value"
  }`,`  node "create" {
    kind = "transform"
    value = "nodes.start.value"
  }`,1)
    src=strings.Replace(src,`    allow_effects = true`,"",1)
    cfg,err:=config.Decode(src)
    if err!=nil {t.Fatal(err)}
    for _,items:=range [][]any{
        {map[string]any{"id":"B"},map[string]any{"id":"A"}},
        {},
    } {
        expected:=make([]any,0,len(items))
        expected=append(expected,items...)
        out,err:=New(nil).Run(context.Background(),"rev",cfg,"mapped","write",
            map[string]any{"records":items},true)
        if err!=nil || !reflect.DeepEqual(out.Value,expected) {
            t.Fatalf("pure mapped child reordered output or failed: %#v %v want=%#v",out,err,expected)
        }
    }
}

func TestMappedCalleesParticipateInTrustAndOAuthDelegation(t *testing.T) {
    cfg,err:=config.Decode(boundedPipelineMapSource("http://localhost:1234"))
    if err!=nil {t.Fatal(err)}
    surface:=cfg.Surfaces["mapped"]
    surface.OAuth="authenticated"
    cfg.Surfaces["mapped"]=surface
    tool:=surface.Tools[0]
    if err:=enforceDataTrust(cfg,"mapped",tool);!errors.Is(err,ErrUntrustedDestination) {
        t.Fatalf("untrusted mapped child bypassed authenticated trust check: %v",err)
    }
    upstream:=cfg.Upstreams["crm"]
    upstream.TrustLevel="restricted"
    upstream.OAuthExchange="delegated"
    cfg.Upstreams["crm"]=upstream
    if err:=enforceDataTrust(cfg,"mapped",tool);err!=nil {
        t.Fatalf("trusted mapped child rejected: %v",err)
    }
    if !ToolNeedsDelegation(cfg,"mapped",tool) {
        t.Fatal("mapped child OAuth exchange invisible to queued task gate")
    }
}
