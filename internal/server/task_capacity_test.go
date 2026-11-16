package server

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "sync"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/storage"
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAsyncTaskConcurrencyBackpressureAndSlotRelease(t *testing.T){
    entered:=make(chan struct{},4)
    release:=make(chan struct{})
    var closeOnce sync.Once
    t.Cleanup(func(){closeOnce.Do(func(){close(release)})})
    upstream:=sdk.NewServer(&sdk.Implementation{Name:"capacity-test",Version:"1"},nil)
    upstream.AddTool(&sdk.Tool{Name:"wait",InputSchema:map[string]any{"type":"object"}},
        func(ctx context.Context,_ *sdk.CallToolRequest)(*sdk.CallToolResult,error) {
            entered<-struct{}{}
            select {
            case <-ctx.Done():return nil,ctx.Err()
            case <-release:
                return &sdk.CallToolResult{
                    Content:[]sdk.Content{&sdk.TextContent{Text:"done"}},
                    StructuredContent:map[string]any{"done":true},
                },nil
            }
        })
    handler:=sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return upstream},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
    external:=httptest.NewServer(handler)
    defer external.Close()

    source:=fmt.Sprintf(`
regate {
  listen = "127.0.0.1:8080"
  max_active_tasks = 1
}
upstream "slow" {
  url = %q
  timeout_ms = 5000
}
surface "work" {
  path = "/mcp/work"
  tool "wait" {
    source = "slow.wait"
    read_only = true
    description = "Block until released"
  }
}
`,external.URL)
    svc,err:=New(t.TempDir(),source)
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()
    started,err:=svc.CreateTask(context.Background(),"work","wait","owner-1",map[string]any{})
    if err!=nil{t.Fatal(err)}
    firstID,ok:=started["taskId"].(string)
    if !ok {t.Fatalf("missing task id: %#v",started)}
    select {
    case <-entered:
    case <-time.After(5*time.Second):t.Fatal("initial task never reached upstream")
    }
    if _,err=svc.CreateTask(context.Background(),"work","wait","owner-2",map[string]any{});
        !errors.Is(err,storage.ErrTaskCapacity){
        t.Fatalf("overloaded service did not apply backpressure: %v",err)
    }
    if n,err:=svc.Store.ActiveTaskCount(context.Background());err!=nil || n!=1 {
        t.Fatalf("overloaded task persisted or count inaccurate: %d %v",n,err)
    }
    closeOnce.Do(func(){close(release)})
    deadline:=time.Now().Add(5*time.Second)
    finished:=false
    for time.Now().Before(deadline) {
        task,err:=svc.Store.TaskGet(firstID)
        if err!=nil{t.Fatal(err)}
        if task.Status=="completed"{finished=true;break}
        time.Sleep(10*time.Millisecond)
    }
    if !finished {t.Fatal("initial task did not complete")}
    second,err:=svc.CreateTask(context.Background(),"work","wait","owner-2",map[string]any{})
    if err!=nil || second["taskId"]==nil {
        t.Fatalf("capacity was not released: %#v %v",second,err)
    }
}
