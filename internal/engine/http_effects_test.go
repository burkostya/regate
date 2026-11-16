package engine

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "path/filepath"
    "strings"
    "sync/atomic"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
)

func effectTestStore(t *testing.T) *storage.Store {
    t.Helper()
    s,err:=storage.New(t.TempDir(),`regate { listen = "127.0.0.1:8080" }`)
    if err!=nil {t.Fatal(err)}
    t.Cleanup(func(){_ = s.Close()})
    if err=s.AcquireExecutionLease();err!=nil {t.Fatal(err)}
    return s
}
func httpEffectContext(run,step string) context.Context {
    return withEffectStep(withEffectScope(context.Background(),run),step)
}

func TestHTTPPostEffectJournalBlocksReplayAndArgumentDrift(t *testing.T) {
    var calls atomic.Int64
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        if r.Method!="POST" {t.Errorf("unexpected request method %s",r.Method)}
        calls.Add(1)
        w.Header().Set("Content-Type","application/json")
        fmt.Fprint(w,`{"value":{"created":true}}`)
    }))
    defer upstream.Close()
    s:=effectTestStore(t)
    runtime:=New(s)
    h:=config.Hook{Name:"create",Transport:"http",URL:upstream.URL,Contract:"transform",TimeoutMS:2000}
    ctx:=httpEffectContext("run-http-create","node:create")
    args:=map[string]any{"id":"C100"}
    result,err:=runtime.runHook(ctx,h,nil,args,false)
    if err!=nil {t.Fatal(err)}
    value,ok:=result.(map[string]any)
    if !ok || value["created"]!=true {t.Fatalf("invalid response: %#v",result)}
    records:=s.Effects()
    if len(records)!=1 || records[0].Status!="confirmed" {t.Fatalf("missing committed effect: %#v",records)}
    if strings.Contains(records[0].Operation,upstream.URL) {t.Fatal("raw HTTP URL leaked into journal")}
    if _,err=runtime.runHook(ctx,h,nil,args,false);!errors.Is(err,storage.ErrEffectAlreadyAttempted) {
        t.Fatalf("confirmed POST replayed: %v",err)
    }
    if _,err=runtime.runHook(ctx,h,nil,map[string]any{"id":"C200"},false);!errors.Is(err,storage.ErrEffectConflict) {
        t.Fatalf("changed logical intent not rejected: %v",err)
    }
    if calls.Load()!=1 {t.Fatalf("expected one POST, got %d",calls.Load())}
    if err=s.Close();err!=nil {t.Fatal(err)}
    restarted,err:=storage.New(s.Dir,`regate { listen = "127.0.0.1:8080" }`)
    if err!=nil {t.Fatal(err)}
    defer restarted.Close()
    if err=restarted.AcquireExecutionLease();err!=nil {t.Fatal(err)}
    _,err=New(restarted).runHook(ctx,h,nil,args,false)
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {t.Fatalf("restarted worker repeated POST: %v",err)}
    if calls.Load()!=1 {t.Fatal("write was replayed after restart")}
}

func TestHTTPPostLostResponseRemainsUnknownAndNeverRetries(t *testing.T) {
    var calls atomic.Int64
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        calls.Add(1) // remote write commits before the response is lost
        conn,_,err:=w.(http.Hijacker).Hijack()
        if err!=nil {t.Error(err);return}
        conn.Close()
    }))
    defer upstream.Close()
    s:=effectTestStore(t)
    runtime:=New(s)
    h:=config.Hook{Name:"write",Transport:"http",URL:upstream.URL,Contract:"transform",TimeoutMS:2000}
    ctx:=httpEffectContext("run-http-lost","node:post")
    args:=map[string]any{"order":42}
    _,err:=runtime.runHook(ctx,h,nil,args,false)
    if err==nil {t.Fatal("lost response appeared successful")}
    records:=s.Effects()
    if len(records)!=1 || records[0].Status!="unknown" {t.Fatalf("ambiguous effect not durable: %#v",records)}
    if _,err=runtime.runHook(ctx,h,nil,args,false);!errors.Is(err,storage.ErrEffectAlreadyAttempted) {
        t.Fatalf("ambiguous POST retried: %v",err)
    }
    if calls.Load()!=1 {t.Fatalf("unexpected duplicate POST count %d",calls.Load())}
}

func TestHTTPPostKnownDenialIsConfirmedEffect(t *testing.T) {
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        w.Header().Set("Content-Type","application/json")
        fmt.Fprint(w,`{"decision":"deny"}`)
    }))
    defer upstream.Close()
    s:=effectTestStore(t)
    h:=config.Hook{Name:"gate",Transport:"http",URL:upstream.URL,Contract:"decision",TimeoutMS:2000}
    _,err:=New(s).runHook(httpEffectContext("run-denied","policy:gate"),h,nil,map[string]any{"safe":true},false)
    if !errors.Is(err,ErrDenied) {t.Fatalf("policy denial missing: %v",err)}
    records:=s.Effects()
    if len(records)!=1 || records[0].Status!="confirmed" {
        t.Fatalf("successful remote decision should be recorded despite local denial: %#v",records)
    }
}

func TestHTTPPostNon200AndBadJSONRemainUnknown(t *testing.T) {
    for _,scenario:=range []struct{name string;status int;body string}{
        {"HTTP502",502,`{"error":"unavailable"}`},
        {"MalformedResponse",200,`{invalid`},
    } {
        t.Run(scenario.name,func(t *testing.T){
            upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
                w.WriteHeader(scenario.status)
                fmt.Fprint(w,scenario.body)
            }))
            defer upstream.Close()
            s:=effectTestStore(t)
            h:=config.Hook{Name:"write",Transport:"http",URL:upstream.URL,Contract:"transform",TimeoutMS:2000}
            _,err:=New(s).runHook(httpEffectContext("run-"+scenario.name,"node:post"),h,nil,map[string]any{"id":"C"},false)
            if err==nil {t.Fatal("uncertain response interpreted as success")}
            records:=s.Effects()
            if len(records)!=1 || records[0].Status!="unknown" {t.Fatalf("state not uncertain: %#v",records)}
        })
    }
}

func TestHTTPPostWithoutJournalCannotDispatch(t *testing.T) {
    var calls atomic.Int64
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        calls.Add(1)
        fmt.Fprint(w,`{"value":true}`)
    }))
    defer upstream.Close()
    h:=config.Hook{Transport:"http",URL:upstream.URL,Contract:"transform",TimeoutMS:2000}
    _,err:=New(nil).runHook(httpEffectContext("orphan","node:post"),h,nil,map[string]any{"id":"C"},false)
    if err==nil || !strings.Contains(err.Error(),"durable effect journal required") {t.Fatalf("unprotected POST was accepted: %v",err)}
    if calls.Load()!=0 {t.Fatal("sent HTTP POST without journal")}
}

func TestMCPHookUsesStableRunIdentity(t *testing.T) {
    upstream,calls:=testEffectUpstream(t,false)
    defer upstream.Close()
    s:=effectTestStore(t)
    cfg:=&config.Config{Upstreams:map[string]config.Upstream{
        "crm":{Name:"crm",URL:upstream.URL,TimeoutMS:2000},
    }}
    h:=config.Hook{Name:"agent",Transport:"mcp",Upstream:"crm",Tool:"create",Contract:"transform"}
    ctx:=httpEffectContext("run-mcp-hook","interceptor:before_call:agent")
    args:=map[string]any{"id":"C"}
    _,err:=New(s).runHook(ctx,h,cfg,args,false)
    if err!=nil {t.Fatal(err)}
    _,err=New(s).runHook(ctx,h,cfg,args,false)
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {t.Fatalf("MCP hook retried: %v",err)}
    if calls.Load()!=1 {t.Fatalf("duplicate MCP tool call: %d",calls.Load())}
}

func TestHTTPPostEffectOnlyStoresFingerprints(t *testing.T) {
    s:=effectTestStore(t)
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        fmt.Fprint(w,`{"value":"ok"}`)
    }))
    defer upstream.Close()
    h:=config.Hook{Transport:"http",URL:upstream.URL+"/sensitive-path",Contract:"transform",TimeoutMS:2000}
    _,err:=New(s).runHook(httpEffectContext("secret-run","node:post"),h,nil,map[string]any{"token":"test-private-value"},false)
    if err!=nil {t.Fatal(err)}
    raw,err:=storage.New(filepath.Dir(filepath.Join(s.Dir,"not-a-file")), `regate { listen = "127.0.0.1:8080" }`)
    if err!=nil {t.Fatal(err)}
    defer raw.Close()
    // Inspect only persisted effect records; the plaintext arguments are not part
    // of the effect journal (other durable domains have separate redaction work).
    records:=raw.Effects()
    if len(records)!=1 || records[0].IntentDigest=="" {t.Fatalf("effect fingerprint missing: %#v",records)}
    if strings.Contains(fmt.Sprint(records),"test-private-value") || strings.Contains(fmt.Sprint(records),"/sensitive-path") {
        t.Fatal("effect journal persisted argument or raw URL")
    }
}
