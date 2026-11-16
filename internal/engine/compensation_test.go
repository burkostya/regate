package engine

import (
    "bytes"
    "context"
    "errors"
    "io"
    "net/http"
    "net/http/httptest"
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func compensationMCPServer(t *testing.T,lost bool)(*httptest.Server,*atomic.Int64) {
    t.Helper()
    calls:=new(atomic.Int64)
    srv:=sdk.NewServer(&sdk.Implementation{Name:"compensation",Version:"1"},nil)
    srv.AddTool(&sdk.Tool{Name:"delete",InputSchema:map[string]any{"type":"object"}},
        func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
            calls.Add(1)
            return &sdk.CallToolResult{
                Content:[]sdk.Content{&sdk.TextContent{Text:"accepted"}},
                StructuredContent:map[string]any{"received":true},
            },nil
        })
    sdkHandler:=sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return srv},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
    handler:=http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        if lost && r.Method==http.MethodPost {
            data,e:=io.ReadAll(io.LimitReader(r.Body,1<<20))
            if e!=nil {http.Error(w,"bad request",400);return}
            r.Body=io.NopCloser(bytes.NewReader(data))
            if bytes.Contains(data,[]byte(`"method":"tools/call"`)) &&
                bytes.Contains(data,[]byte(`"delete"`)) {
                calls.Add(1) // upstream action committed, reply lost
                if hijacker,ok:=w.(http.Hijacker);ok {
                    conn,_,e:=hijacker.Hijack()
                    if e==nil {_=conn.Close();return}
                }
                http.Error(w,"connection closed",502)
                return
            }
        }
        sdkHandler.ServeHTTP(w,r)
    })
    return httptest.NewServer(handler),calls
}
func compensationEngineFixture(t *testing.T,endpoint string)(*Runtime,*config.Config,storage.Effect) {
    t.Helper()
    s:=effectTestStore(t)
    args:=map[string]any{"external_id":"C123"}
    original,err:=s.BeginEffectBound("original-create","create","crm.create",
        storage.TargetDigest("mcp",endpoint),args)
    if err!=nil{t.Fatal(err)}
    if err=s.CompleteEffect(original.ID,"unknown","lost");err!=nil{t.Fatal(err)}
    original,err=s.ResolveByOperator(original.ID,"reviewer","applied","checked remote audit")
    if err!=nil{t.Fatal(err)}
    cfg:=&config.Config{
        Upstreams:map[string]config.Upstream{
            "crm":{Name:"crm",URL:endpoint,TimeoutMS:2000},
        },
        Compensations:map[string]config.Compensation{
            "undo_create":{
                Name:"undo_create",ForOperation:"crm.create",
                Upstream:"crm",Tool:"delete",
                IntentPointer:"/external_id",Argument:"external_id",Enabled:true,
            },
        },
    }
    return New(s),cfg,original
}

func TestMCPCompensationExecutedOnceWithAudit(t *testing.T){
    upstream,count:=compensationMCPServer(t,false)
    defer upstream.Close()
    runtime,cfg,source:=compensationEngineFixture(t,upstream.URL)
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    args:=map[string]any{"external_id":"C123"}
    comp,err:=runtime.CompensateMCP(ctx,cfg,source.ID,"undo_create",args,"op-1","ticket #42")
    if err!=nil {t.Fatal(err)}
    if comp.Status!="confirmed" || comp.Compensates!=source.ID ||
        comp.Approval==nil || comp.Approval.Actor!="op-1" {
        t.Fatalf("missing confirmed compensating effect: %#v",comp)
    }
    if count.Load()!=1 {t.Fatalf("expected 1 compensating call, got %d",count.Load())}
    _,err=runtime.CompensateMCP(ctx,cfg,source.ID,"undo_create",args,"op-1","ticket #42")
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {t.Fatalf("compensation replay accepted: %v",err)}
    if count.Load()!=1 {t.Fatal("duplicate remote compensation")}
    previous,err:=runtime.Store.EffectGet(source.ID)
    if err!=nil || previous.Status!="resolved_applied" {
        t.Fatalf("compensation overwrote original proof: %#v %v",previous,err)
    }
}

func TestMCPCompensationLostResponseCannotReplay(t *testing.T) {
    upstream,count:=compensationMCPServer(t,true)
    defer upstream.Close()
    runtime,cfg,source:=compensationEngineFixture(t,upstream.URL)
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    args:=map[string]any{"external_id":"C123"}
    uncertain,err:=runtime.CompensateMCP(ctx,cfg,source.ID,"undo_create",args,"op-2","explicit approval")
    if err==nil {t.Fatal("lost compensation response reported success")}
    if uncertain.Status!="unknown" || uncertain.Compensates!=source.ID {
        t.Fatalf("lost compensation result was not retained: %#v",uncertain)
    }
    _,err=runtime.CompensateMCP(ctx,cfg,source.ID,"undo_create",args,"op-2","explicit approval")
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {t.Fatalf("uncertain compensation retried: %v",err)}
    if count.Load()!=1 {t.Fatalf("duplicated uncertain inverse: %d",count.Load())}
}

func TestMCPCompensationCannotChangeResourceOrIntent(t *testing.T){
    upstream,count:=compensationMCPServer(t,false)
    defer upstream.Close()
    runtime,cfg,source:=compensationEngineFixture(t,upstream.URL)
    wrong:=map[string]any{"external_id":"OTHER"}
    _,err:=runtime.CompensateMCP(context.Background(),cfg,source.ID,"undo_create",wrong,"operator","approved")
    if !errors.Is(err,storage.ErrInvalidEvidence) {t.Fatalf("incorrect original intent accepted: %v",err)}
    cfg.Upstreams["crm"]=config.Upstream{Name:"crm",URL:"http://127.0.0.1:8888",TimeoutMS:2000}
    _,err=runtime.CompensateMCP(context.Background(),cfg,source.ID,"undo_create",
        map[string]any{"external_id":"C123"},"operator","approved")
    if !errors.Is(err,storage.ErrInvalidEvidence) {t.Fatalf("cross-endpoint compensation accepted: %v",err)}
    if count.Load()!=0 {t.Fatal("unauthorized inverse invoked upstream")}
}
