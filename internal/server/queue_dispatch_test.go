package server

import (
    "bytes"
    "context"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "sync"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/storage"
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func queuedTestKey(t *testing.T) {
    t.Helper()
    key:=sha256.Sum256([]byte("queue integration test key"))
    ring,_:=json.Marshal(map[string]any{
        "active":"k1",
        "keys":map[string]string{"k1":base64.StdEncoding.EncodeToString(key[:])},
    })
    t.Setenv("REGATE_QUEUE_KEYS_JSON",string(ring))
}
func queueServerFixture(t *testing.T)(*httptest.Server,<-chan string,func()) {
    t.Helper()
    entered:=make(chan string,8)
    release:=make(chan struct{})
    var once sync.Once
    unblock:=func(){once.Do(func(){close(release)})}
    t.Cleanup(unblock)
    server:=sdk.NewServer(&sdk.Implementation{Name:"queue-upstream",Version:"1"},nil)
    server.AddTool(&sdk.Tool{Name:"wait",InputSchema:map[string]any{"type":"object"}},
        func(ctx context.Context,call *sdk.CallToolRequest)(*sdk.CallToolResult,error){
            raw,_:=json.Marshal(call.Params.Arguments)
            args:=map[string]any{}
            if err:=json.Unmarshal(raw,&args);err!=nil{return nil,err}
            id,_:=args["id"].(string)
            entered<-id
            select {
            case <-ctx.Done():return nil,ctx.Err()
            case <-release:
                return &sdk.CallToolResult{
                    Content:[]sdk.Content{&sdk.TextContent{Text:"ok"}},
                    StructuredContent:map[string]any{"done":true},
                },nil
            }
        })
    handler:=sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server{return server},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})
    return httptest.NewServer(handler),entered,unblock
}
func queueConfig(url string)string {
    return fmt.Sprintf(`
regate {
  listen = "127.0.0.1:8080"
  max_active_tasks = 1
  max_queued_tasks = 2
}
upstream "backend" {
  url = %q
  timeout_ms = 5000
}
surface "work" {
  path = "/mcp/work"
  tool "wait" {
    source = "backend.wait"
    read_only = true
    description = "Queued execution test"
  }
}
`,url)
}
func startQueueTask(t *testing.T,svc *Service,id string)string{
    t.Helper()
    result,err:=svc.CreateTask(context.Background(),"work","wait","owner",
        map[string]any{"id":id})
    if err!=nil{t.Fatal(err)}
    taskID,ok:=result["taskId"].(string)
    if !ok{t.Fatalf("missing task ID %#v",result)}
    return taskID
}
func awaitQueueEvent(t *testing.T,events <-chan string,expected string){
    t.Helper()
    select{
    case got:=<-events:
        if got!=expected{t.Fatalf("executed %q instead of %q",got,expected)}
    case <-time.After(6*time.Second):t.Fatalf("no upstream call for %s",expected)
    }
}
func awaitQueuedTask(t *testing.T,s *storage.Store,id,status string) {
    t.Helper()
    deadline:=time.Now().Add(6*time.Second)
    for time.Now().Before(deadline) {
        task,err:=s.TaskGet(id)
        if err==nil && task.Status==status{return}
        time.Sleep(10*time.Millisecond)
    }
    task,err:=s.TaskGet(id)
    t.Fatalf("task %s was not %s: %#v %v",id,status,task,err)
}
func TestMCPQueueDispatchFIFOAndCancellation(t *testing.T){
    queuedTestKey(t)
    backend,events,unblock:=queueServerFixture(t)
    defer backend.Close()
    svc,err:=New(t.TempDir(),queueConfig(backend.URL))
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()
    first:=startQueueTask(t,svc,"first")
    awaitQueueEvent(t,events,"first")
    second:=startQueueTask(t,svc,"sensitive-second-input")
    third:=startQueueTask(t,svc,"third")
    if _,err=svc.CreateTask(context.Background(),"work","wait","owner",
        map[string]any{"id":"overflow"});!errors.Is(err,storage.ErrTaskQueueFull){
        t.Fatalf("bounded service queue overflow admitted: %v",err)
    }
    if n,err:=svc.Store.QueueDepth(context.Background());err!=nil || n!=2{
        t.Fatalf("queue depth wrong: %d %v",n,err)
    }
    backup:=filepath.Join(t.TempDir(),"queue.sqlite")
    if err=svc.Store.Backup(backup);err!=nil{t.Fatal(err)}
    snapshot,err:=os.ReadFile(backup)
    if err!=nil{t.Fatal(err)}
    if bytes.Contains(snapshot,[]byte("sensitive-second-input")){
        t.Fatal("queued MCP input leaked as SQLite plaintext")
    }
    if _,err=svc.TaskCancel(second,"owner");err!=nil{t.Fatal(err)}
    awaitQueuedTask(t,svc.Store,second,"cancelled")
    unblock()
    awaitQueueEvent(t,events,"third")
    awaitQueuedTask(t,svc.Store,first,"completed")
    awaitQueuedTask(t,svc.Store,third,"completed")
    select{case extra:=<-events:t.Fatalf("cancelled task executed: %s",extra);default:}
}
func TestQueuedTaskRestartsAndDoesNotReplayClaimedWork(t *testing.T){
    queuedTestKey(t)
    backend,events,unblock:=queueServerFixture(t)
    defer backend.Close()
    source,dir:=queueConfig(backend.URL),t.TempDir()
    old,err:=New(dir,source)
    if err!=nil{t.Fatal(err)}
    first:=startQueueTask(t,old,"first")
    awaitQueueEvent(t,events,"first")
    queued:=startQueueTask(t,old,"recovered")
    if err=old.Store.Close();err!=nil{t.Fatal(err)}
    next,err:=New(dir,source)
    if err!=nil{t.Fatal(err)}
    defer next.Store.Close()
    awaitQueueEvent(t,events,"recovered")
    unblock()
    awaitQueuedTask(t,next.Store,queued,"completed")
    awaitQueuedTask(t,next.Store,first,"failed")
    if depth,err:=next.Store.QueueDepth(context.Background());err!=nil || depth!=0{
        t.Fatalf("replayed or retained queued entry: %d %v",depth,err)
    }
}
func TestQueueRequiresKeyAtServiceStart(t *testing.T){
    t.Setenv("REGATE_QUEUE_KEYS_JSON","")
    backend,_,_:=queueServerFixture(t)
    defer backend.Close()
    svc,err:=New(t.TempDir(),queueConfig(backend.URL))
    if err==nil {svc.Store.Close();t.Fatal("unencrypted queue enabled")}
    if !errors.Is(err,storage.ErrTaskQueueKeyUnavailable){t.Fatalf("missing queue-key error: %v",err)}
}
