package engine

import (
    "context"
    "errors"
    "net/http"
    "net/http/httptest"
    "strings"
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

func TestMCPReadbackRequiresOriginalOperationToken(t *testing.T) {
    operation:="crm.create"
    identity:=storage.EffectID("token-run","create",operation)
    for _,tc:=range []struct{
        name string
        providerTokenMode string
        tamperIntent bool
        mutateContract bool
        wantSuccess bool
        wantReads int64
    }{
        {"committed-token","match",false,false,true,1},
        {"prior-record-identical-payload",strings.Repeat("0",64),false,false,false,1},
        {"provider-does-not-return-token","",false,false,false,1},
        {"caller-forges-reserved-token","match",true,false,false,0},
        {"changed-active-readback-contract","match",false,true,false,0},
    } {
        t.Run(tc.name,func(t *testing.T) {
            reads:=new(atomic.Int64)
            providerToken:=tc.providerTokenMode
            sdkServer:=sdk.NewServer(&sdk.Implementation{Name:"token-test",Version:"1"},nil)
            sdkServer.AddTool(&sdk.Tool{Name:"find",InputSchema:map[string]any{"type":"object"}},
                func(_ context.Context,_ *sdk.CallToolRequest)(*sdk.CallToolResult,error) {
                    reads.Add(1)
                    record:=map[string]any{"external_id":"C123"}
                    if providerToken!="" {record["operation_token"]=providerToken}
                    return &sdk.CallToolResult{StructuredContent:map[string]any{"record":record},
                        Content:[]sdk.Content{&sdk.TextContent{Text:"fixture read"}}},nil
                })
            upstream:=httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return sdkServer},
                &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true}))
            defer upstream.Close()
            store:=effectTestStore(t)
            cfg:=readbackConfig(upstream.URL)
            rule:=cfg.Readbacks["crm_check"]
            rule.WriteTokenArgument="operation_token"
            rule.ResultTokenPointer="/structuredContent/record/operation_token"
            cfg.Readbacks["crm_check"]=rule
            expectedToken,err:=ReadbackOperationToken(identity,rule,cfg.Upstreams["crm"])
            if err!=nil {t.Fatal(err)}
            if expectedToken==identity {t.Fatal("write token ignored contract binding")}
            if tc.providerTokenMode=="match" {providerToken=expectedToken}
            original:=map[string]any{"external_id":"C123"}
            wire,err:=attachWriteToken(original,"operation_token",expectedToken)
            if err!=nil {t.Fatal(err)}
            if _,has:=original["operation_token"];has {t.Fatal("mutated caller original intent")}
            attempt,err:=store.BeginEffectBound("token-run","create",operation,
                storage.TargetDigest("mcp",upstream.URL),wire)
            if err!=nil {t.Fatal(err)}
            if err:=store.CompleteEffect(attempt.ID,"unknown","ambiguous outcome");err!=nil {t.Fatal(err)}
            if tc.tamperIntent {
                original["operation_token"]=expectedToken
            }
            if tc.mutateContract {
                current:=cfg.Readbacks["crm_check"]
                current.ReadTool="another-read-method"
                cfg.Readbacks["crm_check"]=current
            }
            resolved,err:=New(store).ReconcileMCP(context.Background(),cfg,attempt.ID,
                "crm_check",original)
            if tc.wantSuccess {
                if err!=nil || resolved.Status!="resolved_applied" {
                    t.Fatalf("matching operation not resolved: %+v %v",resolved,err)
                }
            } else {
                if !errors.Is(err,storage.ErrInvalidEvidence) {
                    t.Fatalf("untrusted readback accepted: %+v %v",resolved,err)
                }
                saved,err:=store.EffectGet(attempt.ID)
                if err!=nil || saved.Status!="unknown" {
                    t.Fatalf("readback changed uncertain effect: %+v %v",saved,err)
                }
            }
            if got:=reads.Load();got!=tc.wantReads {
                t.Fatalf("external reads=%d want=%d",got,tc.wantReads)
            }
        })
    }
}

func TestReadbackOperationTokenBindsOriginalContract(t *testing.T) {
    original:=readbackConfig("http://127.0.0.1:12001/mcp")
    rule:=original.Readbacks["crm_check"]
    rule.WriteTokenArgument="operation_token"
    rule.ResultTokenPointer="/structuredContent/record/operation_token"
    u:=original.Upstreams["crm"]
    id:=storage.EffectID("run","write","crm.create")
    expected,err:=ReadbackOperationToken(id,rule,u)
    if err!=nil || len(expected)!=64 {t.Fatalf("cannot derive token: %q %v",expected,err)}
    duplicate,err:=ReadbackOperationToken(id,rule,u)
    if err!=nil || duplicate!=expected {t.Fatalf("token is not deterministic: %q %v",duplicate,err)}
	pinned := u
	pinned.ToolSchemaPins = map[string]string{rule.ReadTool: strings.Repeat("c", 64)}
	withPin, err := ReadbackOperationToken(id, rule, pinned)
	if err != nil || withPin != expected {
		t.Fatal("adding a dispatch precondition changed the token for a prior write")
	}

    changes:=[]struct{
        name string
        rule config.Readback
        upstream config.Upstream
        effectID string
    }{
        {"different-effect",rule,u,storage.EffectID("other","write","crm.create")},
    }
    next:=rule;next.ReadTool="find-v2"
    changes=append(changes,struct{name string;rule config.Readback;upstream config.Upstream;effectID string}{"changed-read-tool",next,u,id})
    next=rule;next.ResultPointer="/structuredContent/other"
    changes=append(changes,struct{name string;rule config.Readback;upstream config.Upstream;effectID string}{"changed-result-pointer",next,u,id})
    next=rule;next.Name="renamed-rule"
    changes=append(changes,struct{name string;rule config.Readback;upstream config.Upstream;effectID string}{"renamed-adapter",next,u,id})
    changedUpstream:=u;changedUpstream.URL="http://127.0.0.1:12002/mcp"
    changes=append(changes,struct{name string;rule config.Readback;upstream config.Upstream;effectID string}{"changed-endpoint",rule,changedUpstream,id})
    changedUpstream=u;changedUpstream.TimeoutMS++
    changes=append(changes,struct{name string;rule config.Readback;upstream config.Upstream;effectID string}{"changed-timeout",rule,changedUpstream,id})
    for _,tc:=range changes {
        t.Run(tc.name,func(t *testing.T){
            actual,err:=ReadbackOperationToken(tc.effectID,tc.rule,tc.upstream)
            if err!=nil || actual==expected {
                t.Fatalf("changed contract shared original proof identity: %q %v",actual,err)
            }
        })
    }
}
