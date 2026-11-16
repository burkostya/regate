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

func testEffectUpstream(t *testing.T,lost bool)(*httptest.Server,*atomic.Int64) {
    t.Helper()
    count:=new(atomic.Int64)
    server:=sdk.NewServer(&sdk.Implementation{Name:"effect-test-upstream",Version:"1"},nil)
    server.AddTool(&sdk.Tool{Name:"create",InputSchema:map[string]any{"type":"object"}},
        func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
            count.Add(1)
            return &sdk.CallToolResult{
                Content:[]sdk.Content{&sdk.TextContent{Text:"created"}},
                StructuredContent:map[string]any{"created":true},
            },nil
        })
    handler:=sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return server},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
    wrapped:=http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        if lost && r.Method==http.MethodPost {
            payload,err:=io.ReadAll(io.LimitReader(r.Body,(1<<20)+1))
            if err!=nil {http.Error(w,"bad request",400);return}
            r.Body=io.NopCloser(bytes.NewReader(payload))
            if bytes.Contains(payload,[]byte(`"method":"tools/call"`)) {
                // Simulate the remote side committing the write before its
                // response is lost. The client must not retry this attempt.
                count.Add(1)
                if hj,ok:=w.(http.Hijacker);ok {
                    conn,_,err:=hj.Hijack()
                    if err==nil {_=conn.Close();return}
                }
                http.Error(w,"connection lost",502)
                return
            }
        }
        handler.ServeHTTP(w,r)
    })
    return httptest.NewServer(wrapped),count
}

func TestEffectfulMCPCallCannotReplayAfterConfirmedWrite(t *testing.T) {
    upstream,count:=testEffectUpstream(t,false)
    defer upstream.Close()
    store,err:=storage.New(t.TempDir(),`regate { listen="127.0.0.1:8080" }`)
    if err!=nil{t.Fatal(err)}
    defer store.Close()
    runtime:=New(store)
    u:=config.Upstream{Name:"crm",URL:upstream.URL,TimeoutMS:5000}
    cfg:=&config.Config{Upstreams:map[string]config.Upstream{"crm":u}}
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    out,err:=runtime.mcpCall(ctx,cfg,u,"create",map[string]any{"id":"C123"},"run1","write","write")
    if err!=nil || out["structuredContent"]==nil {
        t.Fatalf("initial external write failed: %v %#v",err,out)
    }
    if count.Load()!=1 {t.Fatalf("unexpected upstream writes: %d",count.Load())}
    _,err=runtime.mcpCall(ctx,cfg,u,"create",map[string]any{"id":"C123"},"run1","write","write")
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
        t.Fatalf("replayed confirmed write: %v",err)
    }
    if count.Load()!=1 {t.Fatalf("duplicate network effect: %d",count.Load())}
}

func TestEffectfulMCPCallLostResponseNeverRetries(t *testing.T){
    upstream,count:=testEffectUpstream(t,true)
    defer upstream.Close()
    store,err:=storage.New(t.TempDir(),`regate { listen="127.0.0.1:8080" }`)
    if err!=nil{t.Fatal(err)}
    defer store.Close()
    runtime:=New(store)
    u:=config.Upstream{Name:"crm",URL:upstream.URL,TimeoutMS:3000}
    cfg:=&config.Config{Upstreams:map[string]config.Upstream{"crm":u}}
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    _,err=runtime.mcpCall(ctx,cfg,u,"create",map[string]any{"id":"C123"},"run3","write","write")
    if err==nil {t.Fatal("lost effect response reported success")}
    if count.Load()!=1 {t.Fatalf("tool unexpectedly retried: %d",count.Load())}
    effects:=store.Effects()
    if len(effects)!=1 || effects[0].Status!="unknown" {
        t.Fatalf("unknown external outcome was lost: %#v",effects)
    }
    _,err=runtime.mcpCall(ctx,cfg,u,"create",map[string]any{"id":"C123"},"run3","write","write")
    if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
        t.Fatalf("lost-response write was repeated: %v",err)
    }
    if count.Load()!=1 {t.Fatalf("repeated uncertain external effect: %d",count.Load())}
}
