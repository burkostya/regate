package engine

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
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

func TestHTTPDecisionRejectsAmbiguousOversizedAndMalformedResponsesWithoutReplay(t *testing.T) {
    badResponses:=[]struct{name,body string}{
        {"duplicate-decision",`{"decision":"deny","decision":"allow"}`},
        {"escaped-duplicate",`{"decision":"deny","deci\u0073ion":"allow"}`},
        {"nested-duplicate",`{"decision":"allow","data":{"verified":true,"verified":false}}`},
        {"trailing-frame",`{"decision":"allow"}{"decision":"deny"}`},
        {"oversized-valid-prefix",`{"decision":"allow"}`+strings.Repeat(" ",(4<<20)+1)},
        {"invalid-utf8",`{"decision":"allow","data":"`+string([]byte{0xff})+`"}`},
    }
    for _,tc:=range badResponses {
        t.Run(tc.name,func(t *testing.T) {
            var calls atomic.Int64
            upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,_ *http.Request) {
                calls.Add(1)
                w.Header().Set("Content-Type","application/json")
                _,_ = w.Write([]byte(tc.body))
            }))
            defer upstream.Close()
            store:=effectTestStore(t)
            hook:=config.Hook{Name:"gate",Transport:"http",URL:upstream.URL,Contract:"decision",TimeoutMS:4000}
            ctx:=httpEffectContext("run-invalid-decision-"+tc.name,"policy:gate")
            _,err:=New(store).runHook(ctx,hook,nil,map[string]any{"safe":true},false)
            if err==nil {t.Fatal("ambiguous HTTP decision granted access")}
            events:=store.Effects()
            if len(events)!=1 || events[0].Status!="unknown" {
                t.Fatalf("uncertain remote response must stay unknown: %#v",events)
            }
            _,err=New(store).runHook(ctx,hook,nil,map[string]any{"safe":true},false)
            if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
                t.Fatalf("ambiguous HTTP effect was replayable: %v",err)
            }
            if calls.Load()!=1 {t.Fatalf("HTTP hook replayed after bad response: %d",calls.Load())}
        })
    }
}

func TestHTTPHookRequestBodyBoundBeforeEffectAdmission(t *testing.T) {
    var calls atomic.Int64
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        calls.Add(1)
        if _,err:=io.Copy(io.Discard,r.Body);err!=nil {
            t.Errorf("read outbound hook request: %v",err)
            return
        }
        w.Header().Set("Content-Type","application/json")
        fmt.Fprint(w,`{"value":{"ok":true}}`)
    }))
    defer upstream.Close()
    store:=effectTestStore(t)
    runtime:=New(store)
    hook:=config.Hook{Name:"transform",Transport:"http",URL:upstream.URL,Contract:"transform",TimeoutMS:15000}

    // Exact-limit payloads are accepted. The envelope size (not only the
    // input value) counts against the wire budget.
    makeInput:=func(length int) map[string]any {
        return map[string]any{"payload":strings.Repeat("x",length)}
    }
    envelope,_:=json.Marshal(map[string]any{"input":makeInput(0)})
    size:=maxHTTPHookRequestBytes-len(envelope)
    atLimit:=makeInput(size)
    encoded,_:=json.Marshal(map[string]any{"input":atLimit})
    if len(encoded)!=maxHTTPHookRequestBytes {
        t.Fatalf("test fixture is not at the actual JSON limit: %d",len(encoded))
    }
    out,err:=runtime.runHook(httpEffectContext("run-request-exact","node:hook"),hook,nil,atLimit,false)
    if err!=nil {t.Fatalf("exact-boundary request rejected: %v",err)}
    if v,ok:=out.(map[string]any);!ok || v["ok"]!=true {t.Fatalf("bad valid-hook result: %#v",out)}
    before:=store.Effects()
    if len(before)!=1 || before[0].Status!="confirmed" || calls.Load()!=1 {
        t.Fatalf("exact-boundary request not durably confirmed: %#v / calls=%d",before,calls.Load())
    }

    // A single byte beyond the bound fails BEFORE BeginEffectBound and before
    // external network I/O, so it cannot create an uncertain external write.
    _,err=runtime.runHook(httpEffectContext("run-request-exceeded","node:hook"),hook,nil,makeInput(size+1),false)
    if err==nil || !strings.Contains(err.Error(),"exceeds") {
        t.Fatalf("oversized request was accepted: %v",err)
    }
    after:=store.Effects()
    if len(after)!=1 || calls.Load()!=1 {
        t.Fatalf("oversized hook dispatched or journaled an effect: %#v / calls=%d",after,calls.Load())
    }
}
