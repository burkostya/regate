package server

import (
    "net/http/httptest"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestMCPTaskExtensionSeesQueuedWorkAndCanCancel(t *testing.T) {
    queuedTestKey(t)
    backend,calls,unblock:=queueServerFixture(t)
    defer backend.Close()
    svc,err:=New(t.TempDir(),queueConfig(backend.URL))
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()
    public:=httptest.NewServer(svc.Handler())
    defer public.Close()
    endpoint:=public.URL+"/mcp/work"
    create:=func(id string,n int)string{
        t.Helper()
        payload:=map[string]any{
            "name":"wait","arguments":map[string]any{"id":id},
            "_meta":map[string]any{
                "io.modelcontextprotocol/protocolVersion":"2026-07-28",
                "io.modelcontextprotocol/clientCapabilities":map[string]any{
                    "extensions":map[string]any{
                        "io.modelcontextprotocol/tasks":map[string]any{},
                    },
                },
            },
        }
        reply:=decode(t,latestCall(t,endpoint,"","tools/call",n,payload))
        if reply["error"]!=nil{t.Fatalf("MCP async call rejected: %#v",reply)}
        data:=reply["result"].(map[string]any)
        if data["resultType"]!="task" {t.Fatalf("missing MCP task extension result: %#v",reply)}
        return data["taskId"].(string)
    }
    first:=create("first",501)
    awaitQueueEvent(t,calls,"first")
    second:=create("queued",502)
    queued:=decode(t,latestCall(t,endpoint,"","tasks/get",503,
        map[string]any{"taskId":second}))
    if queued["error"]!=nil{t.Fatalf("queued task not visible: %#v",queued)}
    state:=queued["result"].(map[string]any)
    if state["status"]!="working" || state["statusMessage"]!="Waiting for available execution capacity"{
        t.Fatalf("bad queued MCP projection: %#v",state)
    }
    cancelled:=decode(t,latestCall(t,endpoint,"","tasks/cancel",504,
        map[string]any{"taskId":second}))
    if cancelled["error"]!=nil {t.Fatalf("queued task cancellation failed: %#v",cancelled)}
    awaitQueuedTask(t,svc.Store,second,"cancelled")
    if depth,err:=svc.Store.QueueDepth(t.Context());err!=nil || depth!=0 {
        t.Fatalf("cancelled queued bytes not removed: %d %v",depth,err)
    }
    unblock()
    awaitQueuedTask(t,svc.Store,first,"completed")
    select {case extra:=<-calls:t.Fatalf("cancelled MCP task executed: %s",extra);default:}
}
