package engine

import (
    "context"
    "errors"
    "net/http"
    "net/http/httptest"
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func readbackTestUpstream(t *testing.T,found bool)(*httptest.Server,*atomic.Int64) {
    t.Helper()
    reads:=new(atomic.Int64)
    server:=sdk.NewServer(&sdk.Implementation{Name:"readback-test",Version:"1"},nil)
    server.AddTool(&sdk.Tool{Name:"find",InputSchema:map[string]any{"type":"object"}},
        func(_ context.Context,req *sdk.CallToolRequest)(*sdk.CallToolResult,error) {
            reads.Add(1)
            externalID:="different"
            if found {externalID="C123"}
            return &sdk.CallToolResult{
                StructuredContent:map[string]any{
                    "record":map[string]any{"external_id":externalID},
                },
                Content:[]sdk.Content{&sdk.TextContent{Text:"lookup"}},
            },nil
        })
    handler:=sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return server},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
    return httptest.NewServer(handler),reads
}
func readbackConfig(endpoint string)*config.Config {
    return &config.Config{
        Upstreams:map[string]config.Upstream{
            "crm":{Name:"crm",URL:endpoint,TimeoutMS:2000},
        },
        Readbacks:map[string]config.Readback{
            "crm_check":{
                Name:"crm_check",WriteOperation:"crm.create",
                Upstream:"crm",ReadTool:"find",IntentPointer:"/external_id",
                ReadArgument:"external_id",
                ResultPointer:"/structuredContent/record/external_id",SafeRead:true,
            },
        },
    }
}
func readbackAttempt(t *testing.T,s *storage.Store,target string) storage.Effect {
    t.Helper()
    attempt,err:=s.BeginEffectBound("run-readback","create","crm.create",target,
        map[string]any{"external_id":"C123"})
    if err!=nil {t.Fatal(err)}
    if err=s.CompleteEffect(attempt.ID,"unknown","lost upstream response");err!=nil {t.Fatal(err)}
    return attempt
}

func TestMCPPositiveReadbackConfirmedWithoutRepeatingWrite(t *testing.T) {
    upstream,reads:=readbackTestUpstream(t,true)
    defer upstream.Close()
    s:=effectTestStore(t)
    cfg:=readbackConfig(upstream.URL)
    attempt:=readbackAttempt(t,s,storage.TargetDigest("mcp",upstream.URL))
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    resolved,err:=New(s).ReconcileMCP(ctx,cfg,attempt.ID,"crm_check",
        map[string]any{"external_id":"C123"})
    if err!=nil {t.Fatal(err)}
    if resolved.Status!="resolved_applied" || resolved.Resolution==nil ||
        resolved.Resolution.Kind!="readback" ||
        resolved.Resolution.Verdict!="applied" ||
        resolved.Resolution.Verifier!="hcl.readback:crm_check" ||
        len(resolved.Resolution.EvidenceDigest)!=64 {
        t.Fatalf("positive readback not bound to journal: %#v",resolved)
    }
    if reads.Load()!=1 {t.Fatalf("unexpected remote read count: %d",reads.Load())}
    _,err=s.BeginEffectBound("run-readback","create","crm.create",
        storage.TargetDigest("mcp",upstream.URL),map[string]any{"external_id":"C123"})
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {t.Fatalf("write allowed replay: %v",err)}
    _,err=New(s).ReconcileMCP(ctx,cfg,attempt.ID,"crm_check",
        map[string]any{"external_id":"C123"})
    if !errors.Is(err,storage.ErrEffectUnresolved) {t.Fatalf("reconciled twice: %v",err)}
    if reads.Load()!=1 {t.Fatal("duplicate remote read after resolution")}
}

func TestMCPReadbackMismatchedPositiveCannotProveAbsence(t *testing.T) {
    upstream,reads:=readbackTestUpstream(t,false)
    defer upstream.Close()
    s:=effectTestStore(t)
    attempt:=readbackAttempt(t,s,storage.TargetDigest("mcp",upstream.URL))
    _,err:=New(s).ReconcileMCP(context.Background(),readbackConfig(upstream.URL),
        attempt.ID,"crm_check",map[string]any{"external_id":"C123"})
    if !errors.Is(err,storage.ErrInvalidEvidence) {
        t.Fatalf("mismatch must remain unknown: %v",err)
    }
    saved,err:=s.EffectGet(attempt.ID)
    if err!=nil || saved.Status!="unknown" {t.Fatalf("negative evidence changed effect: %#v %v",saved,err)}
    if reads.Load()!=1 {t.Fatalf("expected exactly one safe lookup: %d",reads.Load())}
}

func TestMCPReadbackFailClosedBeforeExternalRead(t *testing.T) {
    upstream,reads:=readbackTestUpstream(t,true)
    defer upstream.Close()
    cases:=[]struct{
        name string
        target string
        intent map[string]any
        mutate func(*config.Config)
    }{
        {"wrong-endpoint",storage.TargetDigest("mcp","http://127.0.0.1:9999"),map[string]any{"external_id":"C123"},nil},
        {"unbound-legacy","",map[string]any{"external_id":"C123"},nil},
        {"wrong-original-intent",storage.TargetDigest("mcp",upstream.URL),map[string]any{"external_id":"C124"},nil},
        {"read-not-attested",storage.TargetDigest("mcp",upstream.URL),map[string]any{"external_id":"C123"},func(cfg *config.Config){x:=cfg.Readbacks["crm_check"];x.SafeRead=false;cfg.Readbacks["crm_check"]=x}},
        {"wrong-write-operation",storage.TargetDigest("mcp",upstream.URL),map[string]any{"external_id":"C123"},func(cfg *config.Config){x:=cfg.Readbacks["crm_check"];x.WriteOperation="crm.update";cfg.Readbacks["crm_check"]=x}},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T){
            s:=effectTestStore(t)
            attempt:=readbackAttempt(t,s,tc.target)
            cfg:=readbackConfig(upstream.URL)
            if tc.mutate!=nil {tc.mutate(cfg)}
            _,err:=New(s).ReconcileMCP(context.Background(),cfg,attempt.ID,
                "crm_check",tc.intent)
            if !errors.Is(err,storage.ErrInvalidEvidence) {t.Fatalf("unsafe evidence accepted: %v",err)}
            saved,err:=s.EffectGet(attempt.ID)
            if err!=nil || saved.Status!="unknown" {t.Fatalf("unsafe attempt resolved: %#v %v",saved,err)}
        })
    }
    if reads.Load()!=0 {t.Fatalf("unsafe reconciliation caused external reads: %d",reads.Load())}
}

func TestJSONPointerNoCodeExecution(t *testing.T) {
    source:=map[string]any{"k/v":map[string]any{"~name":[]any{"first","second"}}}
    if v,ok:=jsonPointer(source,"/k~1v/~0name/1");!ok || v!="second" {
        t.Fatalf("escaped pointer failed: %v %v",v,ok)
    }
    for _,pointer:=range []string{"/k~1v/~0name/-1","/k~1v/~0name/01","/k~1v/~0name/3","not-a-pointer"} {
        if _,ok:=jsonPointer(source,pointer);ok {t.Fatalf("unsafe selector accepted: %s",pointer)}
    }
}
