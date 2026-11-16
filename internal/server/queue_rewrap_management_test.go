package server

import (
    "encoding/base64"
    "encoding/json"
    "io"
    "net/http"
    "strings"
    "testing"
)

func TestQueueKeyManagementRequiresAdminBearerAndNeverReturnsKeyBytes(t *testing.T){
    secret:=base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x",32)))
    ring,_:=json.Marshal(map[string]any{
        "active":"k1",
        "keys":map[string]string{"k1":secret},
    })
    t.Setenv("REGATE_QUEUE_KEYS_JSON",string(ring))
    _,api,_:=fixture(t)

    noAuth:=jsonPost(t,api.URL+"/api/queue-rewrap","",map[string]any{})
    noAuth.Body.Close()
    if noAuth.StatusCode!=http.StatusUnauthorized {
        t.Fatalf("unauthorized rewrap admitted: HTTP %d",noAuth.StatusCode)
    }
    anonymousGet,err:=http.Get(api.URL+"/api/queue-key-usage")
    if err!=nil{t.Fatal(err)}
    anonymousGet.Body.Close()
    if anonymousGet.StatusCode!=http.StatusUnauthorized{
        t.Fatalf("anonymous key usage revealed: HTTP %d",anonymousGet.StatusCode)
    }
    req,err:=http.NewRequest(http.MethodGet,api.URL+"/api/queue-key-usage",nil)
    if err!=nil{t.Fatal(err)}
    req.Header.Set("Authorization","Bearer test-secret")
    resp,err:=http.DefaultClient.Do(req)
    if err!=nil{t.Fatal(err)}
    usage:=decode(t,resp)
    if usage["active_key"]!="k1" || usage["total"]!=float64(0) {
        t.Fatalf("wrong admin queue status: %#v",usage)
    }
    report:=decode(t,jsonPost(t,api.URL+"/api/queue-rewrap",
        "test-secret",map[string]any{}))
    if report["rewrapped"]!=float64(0) ||
        report["remaining_old_key"]!=float64(0) ||
        report["active_key"]!="k1" {
        t.Fatalf("empty queue rewrap unexpected: %#v",report)
    }
    rpc:=decode(t,latestCall(t,api.URL+"/mcp/admin","test-secret",
        "tools/call",281,map[string]any{
            "name":"regate_queue_rewrap",
            "arguments":map[string]any{},
        }))
    if rpc["error"]!=nil || rpc["result"]==nil {
        t.Fatalf("admin MCP queue rewrap missing: %#v",rpc)
    }
    serialized,_:=json.Marshal([]any{usage,report,rpc})
    if strings.Contains(string(serialized),secret) {
        t.Fatal("queue key material leaked through admin surface")
    }
    // Public tool catalogs must not expose management-only rewrap commands.
    public:=latestCall(t,api.URL+"/mcp/support","","tools/call",282,
        map[string]any{"name":"regate_queue_rewrap","arguments":map[string]any{}})
    defer public.Body.Close()
    raw,_:=io.ReadAll(public.Body)
    if public.StatusCode==http.StatusOK {
        var envelope map[string]any
        if err:=json.Unmarshal(raw,&envelope);err!=nil {
            t.Fatalf("invalid public rejection response: %v",err)
        }
        if envelope["error"]==nil {
            result,ok:=envelope["result"].(map[string]any)
            if ok && result["isError"]!=true {
                t.Fatal("public surface invoked privileged queue key management")
            }
        }
    }
}
