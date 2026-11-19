package server

import (
    "context"
    "fmt"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/mcp"
)

// Dedicated repeatable HTTP MCP read load against two separately running
// OS-process providers. Every input is synthetic, all state stays in t.TempDir,
// and the test asserts no unintended write amplification under concurrency.
func TestTwoProcessMCPReadLoadIsolation(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","synthetic-local-load-admin")
    root:=t.TempDir()
    alpha:=startStandProcess(t,"alpha",filepath.Join(root,"alpha"))
    beta:=startStandProcess(t,"beta",filepath.Join(root,"beta"))
    data,err:=os.ReadFile("../../examples/two-mcp-stand.hcl")
    if err!=nil {t.Fatal(err)}
    source:=strings.ReplaceAll(string(data),"http://127.0.0.1:9201",
        strings.TrimSuffix(alpha.url,"/mcp"))
    source=strings.ReplaceAll(source,"http://127.0.0.1:9202",
        strings.TrimSuffix(beta.url,"/mcp"))
    svc,err:=New(filepath.Join(root,"gateway"),source)
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    httpGateway:=httptest.NewServer(svc.Handler())
    defer httpGateway.Close()
    base:=httpGateway.URL

    // One authorized write; the steady-state test must remain read-only.
    original:=standCall(t,base,"alpha","alpha_put",map[string]any{
        "record_id":"load-seed","value":"synthetic",
    })
    if original["isError"]==true {t.Fatalf("failed to prepare synthetic seed: %#v",original)}

    const workers=8
    const readsPerWorker=8
    ctx,cancel:=context.WithTimeout(context.Background(),60*time.Second)
    defer cancel()
    var wg sync.WaitGroup
    failures:=make(chan error,workers*readsPerWorker)
    for i:=0;i<workers;i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            conn:=mcp.NewConn(base+"/mcp/alpha","",10*time.Second)
            for n:=0;n<readsPerWorker;n++ {
                result,err:=conn.CallTool(ctx,"alpha_get",map[string]any{
                    "record_id":"load-seed",
                })
                if err!=nil {failures<-err;return}
                if result["isError"]==true {
                    failures<-fmt.Errorf("read error after %d operations: %v",n,result)
                    return
                }
            }
        }()
    }
    wg.Wait()
    close(failures)
    for err:=range failures {if err!=nil {t.Fatal(err)}}

    // Denied policy branches cannot dispatch a protected read or write.
    before:=standStats(t,base,"alpha")["reads"]
    for i:=0;i<3;i++ {
        denied:=standCall(t,base,"workflow","copy",map[string]any{
            "record_id":"DENY-load-seed",
        })
        if denied["isError"]!=true {t.Fatal("policy denial bypassed under load")}
    }
    after:=standStats(t,base,"alpha")["reads"]
    if before!=after {t.Fatalf("denied branch performed protected reads: %v -> %v",before,after)}
    assertStandCount(t,standStats(t,base,"alpha"),"writes_committed",1)
    assertStandCount(t,standStats(t,base,"beta"),"writes_committed",0)
    if len(svc.Store.Effects())!=1 {t.Fatalf("read load fabricated external effects: %v",svc.Store.Effects())}
    t.Log("64 read calls under 8 concurrent clients, 2 live OS-process providers, no write amplification")
}
