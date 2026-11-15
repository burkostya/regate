package server

import (
    "bytes"
    "context"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "sync"
    "testing"
    "time"
)

// Exercise the raw HTTP contract rather than depending solely on a client that
// uses the same SDK implementation. All wire metadata is supplied explicitly.
func conformancePost(t *testing.T, endpoint, token, version, method, name string, params map[string]any) (int,map[string]any) {
    t.Helper()
    body,err:=json.Marshal(map[string]any{"jsonrpc":"2.0","id":41,"method":method,"params":params})
    if err!=nil {t.Fatal(err)}
    req,err:=http.NewRequest(http.MethodPost,endpoint,bytes.NewReader(body))
    if err!=nil {t.Fatal(err)}
    req.Header.Set("Content-Type","application/json")
    req.Header.Set("Accept","application/json, text/event-stream")
    if version!="" {req.Header.Set("Mcp-Protocol-Version",version)}
    if version=="2026-07-28" {
        req.Header.Set("Mcp-Method",method)
        if name!="" {req.Header.Set("Mcp-Name",name)}
    }
    if token!="" {req.Header.Set("Authorization","Bearer "+token)}
    resp,err:=http.DefaultClient.Do(req)
    if err!=nil {t.Fatal(err)}
    defer resp.Body.Close()
    data,err:=io.ReadAll(resp.Body)
    if err!=nil {t.Fatal(err)}
    var value map[string]any
    if err:=json.Unmarshal(data,&value);err!=nil {t.Fatalf("%s HTTP %d is not JSON-RPC: %s (%v)",method,resp.StatusCode,data,err)}
    return resp.StatusCode,value
}

func TestMCPRawModernAndLegacyHTTPConformance(t *testing.T) {
    _,srv,_:=fixture(t)
    modernMeta:=map[string]any{"_meta":map[string]any{
        "io.modelcontextprotocol/protocolVersion":"2026-07-28",
        "io.modelcontextprotocol/clientCapabilities":map[string]any{},
    }}
    cases:=[]struct{method,name string;params map[string]any;want string}{
        {"server/discover","",modernMeta,"supportedVersions"},
        {"tools/list","",modernMeta,"tools"},
        {"resources/list","",modernMeta,"resources"},
        {"resources/templates/list","",modernMeta,"resourceTemplates"},
        {"prompts/list","",modernMeta,"prompts"},
        {"resources/read","regate://support/instructions",map[string]any{
            "uri":"regate://support/instructions", "_meta":modernMeta["_meta"],
        },"contents"},
        {"prompts/get","customer_review",map[string]any{
            "name":"customer_review", "_meta":modernMeta["_meta"],
        },"messages"},
        {"ping","",modernMeta,""},
    }
    for _,tc:=range cases {
        t.Run(tc.method,func(t *testing.T){
            code,response:=conformancePost(t,srv.URL+"/mcp/support","","2026-07-28",tc.method,tc.name,tc.params)
            if code!=200 || response["error"]!=nil {t.Fatalf("HTTP %d %v",code,response)}
            result,ok:=response["result"].(map[string]any)
            if !ok {t.Fatalf("missing result: %v",response)}
            if tc.want!="" && result[tc.want]==nil {t.Fatalf("missing %s: %v",tc.want,response)}
        })
    }
    oldInit:=map[string]any{
        "protocolVersion":"2025-06-18",
        "capabilities":map[string]any{},
        "clientInfo":map[string]any{"name":"independent-http-client","version":"1"},
    }
    code,res:=conformancePost(t,srv.URL+"/mcp/support","","2025-06-18","initialize","",oldInit)
    if code!=200 || res["error"]!=nil {t.Fatalf("legacy init: HTTP %d %v",code,res)}
    if v:=res["result"].(map[string]any)["protocolVersion"]; v!="2025-06-18" {
        t.Fatalf("legacy negotiation returned %v",v)
    }
    code,res=conformancePost(t,srv.URL+"/mcp/support","","2025-06-18","tools/list","",map[string]any{})
    if code!=200 || res["error"]!=nil {t.Fatalf("legacy tools/list: HTTP %d %v",code,res)}
    code,res=conformancePost(t,srv.URL+"/mcp/admin","test-secret","2026-07-28","skills/list","",modernMeta)
    if code!=200 || res["error"]!=nil || res["result"].(map[string]any)["skills"]==nil {
        t.Fatalf("admin extension interoperability: HTTP %d %v",code,res)
    }
    // The separate admin boundary applies to raw modern clients as well.
    response:=jsonPost(t,srv.URL+"/mcp/admin","",map[string]any{"jsonrpc":"2.0","id":5,"method":"tools/list"})
    defer response.Body.Close()
    if response.StatusCode!=http.StatusUnauthorized {t.Fatalf("anonymous admin access: HTTP %d",response.StatusCode)}
}

func TestMCPRawHTTPProtocolErrorsAndBodyLimit(t *testing.T) {
    _,srv,_:=fixture(t)
    meta:=map[string]any{"_meta":map[string]any{
        "io.modelcontextprotocol/protocolVersion":"2026-07-28",
        "io.modelcontextprotocol/clientCapabilities":map[string]any{},
    }}
    code,response:=conformancePost(t,srv.URL+"/mcp/support","","2026-07-28","unknown/custom","",meta)
    if code < 400 || response["error"]==nil {
        t.Fatalf("unknown method was not rejected: HTTP %d %v",code,response)
    }
    // Do not allow unbounded body parsing even for a nominally valid method.
    huge:=bytes.Repeat([]byte("x"),(4<<20)+1)
    req,err:=http.NewRequest(http.MethodPost,srv.URL+"/mcp/support",bytes.NewReader(huge))
    if err!=nil {t.Fatal(err)}
    req.Header.Set("Content-Type","application/json")
    req.Header.Set("Accept","application/json, text/event-stream")
    resp,err:=http.DefaultClient.Do(req)
    if err!=nil {t.Fatal(err)}
    defer resp.Body.Close()
    if resp.StatusCode!=http.StatusRequestEntityTooLarge {
        t.Fatalf("oversized MCP request: HTTP %d",resp.StatusCode)
    }
}

func TestInboundCancelledRequestStopsBeforeProtectedEffects(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    world:=&mockWorld{}
    entered:=make(chan struct{})
    release:=make(chan struct{})
    var once sync.Once
    upstream:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        if r.URL.Path=="/decision" {
            once.Do(func(){close(entered)})
            select {
            case <-r.Context().Done():
            case <-release:
            }
            return
        }
        world.ServeHTTP(w,r)
    }))
    // A malformed cancellation implementation must fail a test rather than
    // hanging httptest.Server.Close until the full Go test timeout.
    defer func(){close(release);upstream.Close()}()
    source,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    cfg:=strings.ReplaceAll(string(source),"http://127.0.0.1:9091",upstream.URL)
    svc,err:=New(t.TempDir(),cfg)
    if err!=nil {t.Fatal(err)}
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()

    ctx,cancel:=context.WithCancel(context.Background())
    defer cancel()
    params:=map[string]any{"name":"summarize_customer","arguments":map[string]any{"customer_id":"C123"},
        "_meta":map[string]any{
          "io.modelcontextprotocol/protocolVersion":"2026-07-28",
          "io.modelcontextprotocol/clientCapabilities":map[string]any{},
        }}
    payload,err:=json.Marshal(map[string]any{"jsonrpc":"2.0","id":71,"method":"tools/call","params":params})
    if err!=nil {t.Fatal(err)}
    req,err:=http.NewRequestWithContext(ctx,http.MethodPost,api.URL+"/mcp/support",bytes.NewReader(payload))
    if err!=nil {t.Fatal(err)}
    req.Header.Set("Content-Type","application/json")
    req.Header.Set("Accept","application/json, text/event-stream")
    req.Header.Set("Mcp-Protocol-Version","2026-07-28")
    req.Header.Set("Mcp-Method","tools/call")
    req.Header.Set("Mcp-Name","summarize_customer")
    result:=make(chan error,1)
    go func(){
        resp,err:=http.DefaultClient.Do(req)
        if resp!=nil {resp.Body.Close()}
        result<-err
    }()
    select {
    case <-entered: cancel()
    case <-time.After(3*time.Second): t.Fatal("protected policy hook did not start")
    }
    select {
    case <-result:
    case <-time.After(3*time.Second): t.Fatal("cancellation did not complete HTTP request")
    }
    // If the first guarded node returns a cancelled context, the engine must
    // never enter the downstream CRM tool and thus cannot create an effect.
    world.mu.Lock()
    defer world.mu.Unlock()
    for _,method:=range world.calls {
        if method=="tools/call" {t.Fatalf("cancelled request reached protected MCP effect: %v",world.calls)}
    }
}
