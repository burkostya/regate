package engine

import (
    "context"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "reflect"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
)

func suspendableMappedWriterSource(url string) string {
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
  node "pause" {
    kind = "suspend"
    output_schema = {type = "string"}
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
    to = "pause"
  }
  edge {
    from = "pause"
    to = "done"
  }
}
pipeline "batch" {
  entry = "each"
  node "each" {
    kind = "map"
    items = "input.records"
    pipeline = "writer"
    allow_effects = true
    allow_suspend = true
    max_items = 3
    input_schema = {type = "object"}
    output_schema = {type = "array", items = {type = "object"}}
  }
  node "done" {
    kind = "output"
    value = "nodes.each.value"
  }
  edge {
    from = "each"
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

func TestMappedSuspensionsRestoreFrozenPrefixAfterSQLiteRestart(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,false)
    defer upstream.Close()
    cfg,err:=config.Decode(suspendableMappedWriterSource(upstream.URL))
    if err!=nil {t.Fatal(err)}
    dir:=t.TempDir()
    openStore:=func() *storage.Store {
        s,err:=storage.New(dir,`regate { listen = "127.0.0.1:8080" }`)
        if err!=nil {t.Fatal(err)}
        if err:=s.AcquireExecutionLease();err!=nil {t.Fatal(err)}
        return s
    }
    store:=openStore()
    defer func(){_ = store.Close()}()
    args:=map[string]any{"records":[]any{map[string]any{"id":"A"},map[string]any{"id":"B"}}}
    initial,err:=New(store).Run(context.Background(),"rev",cfg,"mapped","write",args,false)
    if err==nil || initial.Status!="suspended" || calls.Load()!=1 {
        t.Fatalf("expected first effectful mapped suspension: %#v %v writes=%d",initial,err,calls.Load())
    }
    if !reflect.DeepEqual(initial.SuspensionPaths,[][]string{{"each","0","pause"}}) {
        t.Fatalf("wrong mapped initial path: %#v",initial.SuspensionPaths)
    }
    var cp Checkpoint
    if err:=store.GetSuspended(initial.RunID,&cp);err!=nil {t.Fatal(err)}
    if cp.SuspendNode!="each" || cp.Child==nil || cp.Child.FrameKind!="map" ||
        cp.Child.MapIndex!=0 || len(cp.Child.MapItems)!=2 || len(cp.Child.MapResults)!=0 {
        t.Fatalf("incorrect first per-item frame: %#v",cp)
    }
    if err:=store.Close();err!=nil {t.Fatal(err)}
    store=openStore()
    // Resume the first leaf after a full database/leader and runtime reopen.
    first,err:=New(store).ResumeAt(context.Background(),cfg,"rev",initial.RunID,
        []string{"each","0","pause"},"accepted")
    if err==nil || first.Status!="suspended" || calls.Load()!=2 {
        t.Fatalf("first item not resumed or next item write repeated: %#v %v writes=%d",first,err,calls.Load())
    }
    if !reflect.DeepEqual(first.SuspensionPaths,[][]string{{"each","1","pause"}}) {
        t.Fatalf("second item's path was lost: %#v",first.SuspensionPaths)
    }
    if err:=store.GetSuspended(initial.RunID,&cp);err!=nil {t.Fatal(err)}
    if cp.Child==nil || cp.Child.FrameKind!="map" || cp.Child.MapIndex!=1 ||
        len(cp.Child.MapResults)!=1 {
        t.Fatalf("first result not frozen in resumed checkpoint: %#v",cp)
    }
    if _,err:=New(store).ResumeAt(context.Background(),cfg,"rev",initial.RunID,
        []string{"each","1","pause"},42);err==nil ||
        !strings.Contains(err.Error(),"schema") {
        t.Fatalf("invalid mapped leaf answer accepted: %v",err)
    }
    if calls.Load()!=2 {t.Fatal("invalid answer redispatched an external write")}
    finished,err:=New(store).ResumeAt(context.Background(),cfg,"rev",initial.RunID,
        []string{"each","1","pause"},"accepted")
    if err!=nil || finished.Status!="success" || calls.Load()!=2 {
        t.Fatalf("final mapped resume failed or replayed effects: %#v %v writes=%d",finished,err,calls.Load())
    }
    want:=[]any{map[string]any{"created":true},map[string]any{"created":true}}
    if !reflect.DeepEqual(finished.Value,want) {
        t.Fatalf("mapped final output missing completed prefix: %#v",finished.Value)
    }
    if err:=store.GetSuspended(initial.RunID,&cp);err!=sql.ErrNoRows {
        t.Fatalf("root checkpoint not consumed after full completion: %v",err)
    }
    effects:=store.Effects()
    if len(effects)!=2 || effects[0].RunID==effects[1].RunID {
        t.Fatalf("mapped effects have no distinct durable identities: %#v",effects)
    }
    _,err=New(store).execute(withEffectScope(context.Background(),initial.RunID),"rev",cfg,
        cfg.Pipelines["batch"],initial.RunID,"mapped","write",args,false,nil)
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) || calls.Load()!=2 {
        t.Fatalf("already confirmed mapped write replayed: %v writes=%d",err,calls.Load())
    }
}

func TestMappedCheckpointRejectsTamperedIndexPrefixAndArguments(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,false)
    defer upstream.Close()
    cfg,err:=config.Decode(suspendableMappedWriterSource(upstream.URL))
    if err!=nil {t.Fatal(err)}
    store:=effectTestStore(t)
    args:=map[string]any{"records":[]any{map[string]any{"id":"A"},map[string]any{"id":"B"}}}
    started,err:=New(store).Run(context.Background(),"rev",cfg,"mapped","write",args,false)
    if err==nil || started.Status!="suspended" || calls.Load()!=1 {
        t.Fatalf("fixture failed: %#v %v",started,err)
    }
    var original Checkpoint
    if err:=store.GetSuspended(started.RunID,&original);err!=nil {t.Fatal(err)}
    for _,tc:=range []struct{name string; mutate func(*Checkpoint)}{
        {"changed iteration index",func(cp *Checkpoint){cp.Child.MapIndex=1}},
        {"forged completed result",func(cp *Checkpoint){cp.Child.MapResults=[]any{map[string]any{"created":true}}}},
        {"changed original later item",func(cp *Checkpoint){cp.Child.MapItems[1].(map[string]any)["id"]=42}},
        {"invalid wrapper kind",func(cp *Checkpoint){cp.Child.FrameKind="pipeline"}},
        {"changed nested subrun",func(cp *Checkpoint){cp.Child.Child.RunID="different"}},
    } {
        t.Run(tc.name,func(t *testing.T){
            raw,err:=json.Marshal(original)
            if err!=nil {t.Fatal(err)}
            var altered Checkpoint
            if err:=json.Unmarshal(raw,&altered);err!=nil {t.Fatal(err)}
            tc.mutate(&altered)
            if err:=store.PutSuspended(started.RunID,&altered);err!=nil {t.Fatal(err)}
            out,err:=New(store).ResumeAt(context.Background(),cfg,"rev",started.RunID,
                []string{"each","0","pause"},"accepted")
            if err==nil || out.Status!="failure" || calls.Load()!=1 {
                t.Fatalf("tampered mapped continuation was executed: %#v %v writes=%d",out,err,calls.Load())
            }
            if err:=store.PutSuspended(started.RunID,&original);err!=nil {t.Fatal(err)}
        })
    }
    finished,err:=New(store).ResumeAt(context.Background(),cfg,"rev",started.RunID,
        []string{"each","0","pause"},"accepted")
    if err==nil || finished.Status!="suspended" || calls.Load()!=2 {
        t.Fatalf("valid restored continuation failed: %#v %v writes=%d",finished,err,calls.Load())
    }
}

func TestMappedSuspendActivationRequiresBothOptInsAndTypedTerminal(t *testing.T) {
    src:=suspendableMappedWriterSource("http://localhost:1234")
    tests:=[]struct{name,source,expected string}{
        {"no allow_suspend",strings.Replace(src,`    allow_suspend = true`,"",1),"suspend requires"},
        {"no allow_effects",strings.Replace(src,`    allow_effects = true`,"",1),"allow_suspend requires"},
        {"no typed output",strings.Replace(src,`    output_schema = {type = "object"}`,"",1),
            "requires child output_schema"},
    }
    for _,tc:=range tests {
        t.Run(tc.name,func(t *testing.T){
            if _,err:=config.Decode(tc.source);err==nil ||
                !strings.Contains(err.Error(),tc.expected) {
                t.Fatalf("wanted %q, got %v",tc.expected,err)
            }
        })
    }
}
