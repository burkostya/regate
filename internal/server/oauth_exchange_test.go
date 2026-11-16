package server

import (
    "crypto/rand"
    "crypto/rsa"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "math/big"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "sync"
    "testing"
    "time"
)

func TestDelegatedUpstreamTokenAudienceAndEffectBoundary(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    t.Setenv("REGATE_OAUTH_EXCHANGE_ID","regate-client")
    t.Setenv("REGATE_OAUTH_EXCHANGE_SECRET","exchange-secret")
    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil {t.Fatal(err)}
    world:=&mockWorld{}
    backend:=httptest.NewServer(world)
    defer backend.Close()
    const issuer="https://issuer.test"
    const regateAudience="https://regate.test/mcp/support"
    const crmAudience="https://crm.test/resource"
    var mu sync.Mutex
    variant:="valid"
    exchanged:=""
    idp:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        switch r.URL.Path {
        case "/jwks":
            json.NewEncoder(w).Encode(map[string]any{"keys":[]any{map[string]any{
                "kid":"test-key","kty":"RSA","alg":"RS256","use":"sig",
                "n":base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
                "e":base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.PublicKey.E)).Bytes()),
            }}})
        case "/token":
            username,password,ok:=r.BasicAuth()
            if !ok || username!="regate-client" || password!="exchange-secret" {
                http.Error(w,"bad credentials",401);return
            }
            if r.ParseForm()!=nil || r.Form.Get("audience")!=crmAudience ||
                r.Form.Get("scope")!="crm:read" ||
                r.Form.Get("subject_token_type")!="urn:ietf:params:oauth:token-type:access_token" {
                http.Error(w,"bad audience",400);return
            }
            mu.Lock()
            mode:=variant
            mu.Unlock()
            audience:=crmAudience
            if mode=="wrong-audience" {audience=regateAudience}
            delegated:=signedTestJWT(t,private,issuer,audience,"crm:read")
            mu.Lock()
            exchanged=delegated
            mu.Unlock()
            json.NewEncoder(w).Encode(map[string]any{
                "access_token":delegated,
                "token_type":"Bearer",
                "issued_token_type":"urn:ietf:params:oauth:token-type:access_token",
                "expires_in":3600,
            })
        default:
            http.NotFound(w,r)
        }
    }))
    defer idp.Close()
    source,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    hcl:=strings.ReplaceAll(string(source),"http://127.0.0.1:9091",backend.URL)
    for _,name:=range []string{`upstream "agent" {`,`hook "decision" {`,`hook "mask_pii" {`,`hook "summarizer" {`} {
        hcl=strings.Replace(hcl,name,name+"\n  trust_level = \"restricted\"",1)
    }
    hcl=strings.Replace(hcl,`upstream "crm" {`,fmt.Sprintf(`upstream "crm" {
      trust_level = "restricted"
      oauth_exchange = "company"
      oauth_audience = %q
      oauth_scopes = ["crm:read"]`,crmAudience),1)
    hcl=strings.Replace(hcl,`surface "support" {
  path = "/mcp/support"`,`surface "support" {
  path = "/mcp/support"
  oauth = "company"
  required_scopes = ["surface:read"]`,1)
    hcl=strings.Replace(hcl,`  tool "get_customer_direct" {`,`  tool "get_customer_direct" {
    data_classification = "restricted"
    required_scopes = ["crm:read"]`,1)
    hcl=fmt.Sprintf(`oauth "company" {
      issuer = %q
      jwks_url = %q
      audience = %q
      token_endpoint = %q
      exchange_client_id_env = "REGATE_OAUTH_EXCHANGE_ID"
      exchange_client_secret_env = "REGATE_OAUTH_EXCHANGE_SECRET"
    }
    `,issuer,idp.URL+"/jwks",regateAudience,idp.URL+"/token")+hcl
    // A pipeline that intentionally suspends before calling a delegated
    // upstream lets us exercise the new-bearer resume path end to end.
    hcl=strings.Replace(hcl,`  tool "get_customer_direct" {`,`
  tool "deferred_customer" {
    pipeline = "deferred_customer"
    required_scopes = ["crm:read"]
    data_classification = "restricted"
  }
  tool "get_customer_direct" {`,1)
    hcl+=`
pipeline "deferred_customer" {
  entry = "wait"
  node "wait" {
    kind = "suspend"
  }
  node "fetch" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "get_customer"
    effect = "read"
    arguments = { customer_id = "input.customer_id" }
  }
  node "result" {
    kind = "output"
    value = "nodes.fetch.value"
  }
  edge {
    from = "wait"
    to = "fetch"
  }
  edge {
    from = "fetch"
    to = "result"
  }
}
`
    svc,err:=New(t.TempDir(),hcl)
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()
    origin:=signedTestJWT(t,private,issuer,regateAudience,"surface:read crm:read")
    args:=map[string]any{"name":"get_customer_direct","arguments":map[string]any{"customer_id":"C1"}}
    reply:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",51,args))
    if reply["error"]!=nil || reply["result"].(map[string]any)["isError"]==true {
        t.Fatalf("delegated call failed: %#v",reply)
    }
    mu.Lock()
    target:=exchanged
    mu.Unlock()
    world.mu.Lock()
    auth:=world.lastAuth
    callsBefore:=len(world.calls)
    world.mu.Unlock()
    if target==origin || !strings.HasPrefix(target,"eyJ") ||
        auth!="Bearer "+target || auth=="Bearer "+origin {
        t.Fatalf("wrong-audience or raw subject token forwarded: backend auth=%q",auth)
    }
    effectsBefore:=len(svc.Store.Effects())
    mu.Lock()
    variant="wrong-audience"
    mu.Unlock()
    denied:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",52,args))
    if denied["result"].(map[string]any)["isError"]!=true {
        t.Fatalf("accepted incorrectly exchanged OAuth token: %#v",denied)
    }
    world.mu.Lock()
    callsAfter:=len(world.calls)
    world.mu.Unlock()
    if callsAfter!=callsBefore || len(svc.Store.Effects())!=effectsBefore {
        t.Fatalf("invalid delegation reached an external effect: before=%d after=%d",callsBefore,callsAfter)
    }
    // Async execution may hold the verified bearer IN MEMORY while live;
    // the persisted task must contain only the owner and provider binding.
    async:=map[string]any{"name":"get_customer_direct","arguments":map[string]any{"customer_id":"C1"},
        "_meta":map[string]any{
            "io.modelcontextprotocol/protocolVersion":"2026-07-28",
            "io.modelcontextprotocol/clientCapabilities":map[string]any{
                "extensions":map[string]any{"io.modelcontextprotocol/tasks":map[string]any{}},
            },
        },
    }
    mu.Lock()
    variant="valid"
    mu.Unlock()
    start:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",53,async))
    if start["error"]!=nil {t.Fatalf("delegated async creation denied: %#v",start)}
    handle:=start["result"].(map[string]any)
    taskID,ok:=handle["taskId"].(string)
    if !ok || taskID=="" {t.Fatalf("async handle absent: %#v",start)}
    deadline:=time.Now().Add(4*time.Second)
    done:=false
    for time.Now().Before(deadline) {
        response:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tasks/get",54,
            map[string]any{"taskId":taskID}))
        task:=response["result"].(map[string]any)
        if task["status"]=="completed" {
            result:=task["result"].(map[string]any)
            if result["isError"]==true {t.Fatalf("delegated async execution failed: %#v",task)}
            done=true
            break
        }
        time.Sleep(10*time.Millisecond)
    }
    if !done {t.Fatal("delegated async task did not complete")}
    recorded,err:=svc.Store.TaskGet(taskID)
    if err!=nil {t.Fatal(err)}
    if recorded.Delegation==nil || recorded.Delegation.Provider!="company" ||
        recorded.Delegation.Audience!=regateAudience {
        t.Fatalf("durable delegation requirement missing: %#v",recorded)
    }
    serialized,_:=json.Marshal(recorded)
    if strings.Contains(string(serialized),origin) ||
        strings.Contains(string(serialized),exchanged) {
        t.Fatal("delegated bearer persisted in SQLite task")
    }
    // An invalid exchanged audience can no longer create an external write,
    // even if the async handle itself was admitted before the exchange.
    world.mu.Lock()
    writesBefore:=len(world.calls)
    world.mu.Unlock()
    mu.Lock()
    variant="wrong-audience"
    mu.Unlock()
    deniedStart:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",55,async))
    if deniedStart["error"]!=nil {t.Fatal(deniedStart)}
    deniedID:=deniedStart["result"].(map[string]any)["taskId"].(string)
    done=false
    deadline=time.Now().Add(4*time.Second)
    for time.Now().Before(deadline) {
        response:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tasks/get",56,
            map[string]any{"taskId":deniedID}))
        task:=response["result"].(map[string]any)
        if task["status"]=="completed" {
            result:=task["result"].(map[string]any)
            if result["isError"]!=true {t.Fatal("invalid delegation succeeded asynchronously")}
            done=true
            break
        }
        time.Sleep(10*time.Millisecond)
    }
    if !done {t.Fatal("invalid delegation task did not resolve")}
    world.mu.Lock()
    writesAfter:=len(world.calls)
    world.mu.Unlock()
    if writesAfter!=writesBefore {t.Fatal("invalid async delegation reached upstream")}

    // Suspend under one bearer, then resume with a different verified
    // bearer for the same subject. The subject token must not be persisted.
    mu.Lock()
    variant="valid"
    mu.Unlock()
    deferred:=map[string]any{
        "name":"deferred_customer",
        "arguments":map[string]any{"customer_id":"C1"},
        "_meta":async["_meta"],
    }
    started:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",60,deferred))
    if started["error"]!=nil {t.Fatalf("deferred task start denied: %#v",started)}
    suspendedID:=started["result"].(map[string]any)["taskId"].(string)
    deadline=time.Now().Add(4*time.Second)
    ready:=false
    for time.Now().Before(deadline) {
        state:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tasks/get",61,map[string]any{"taskId":suspendedID}))
        if state["result"].(map[string]any)["status"]=="input_required" {ready=true;break}
        time.Sleep(10*time.Millisecond)
    }
    if !ready {t.Fatal("delegated pipeline did not suspend")}
    update:=map[string]any{
        "taskId":suspendedID,
        "inputResponses":map[string]any{
            "resume":map[string]any{"action":"accept","content":map[string]any{"approved":true}},
        },
    }
    lostScope:=signedTestJWT(t,private,issuer,regateAudience,"surface:read")
    noScope:=decode(t,latestCall(t,api.URL+"/mcp/support",lostScope,"tasks/update",62,update))
    if noScope["error"]==nil {t.Fatal("revoked crm scope resumed delegated task")}
    other:=signedTestJWTSubject(t,private,issuer,regateAudience,
        "surface:read crm:read","other-user")
    noOwner:=latestCall(t,api.URL+"/mcp/support",other,"tasks/update",63,update)
    noOwner.Body.Close()
    if noOwner.StatusCode<400 {t.Fatal("another principal resumed delegated task")}
    // Nondelegated compatibility API has no OAuth context and must be
    // unable to continue a delegated task despite a matching owner digest.
    storedTask,err:=svc.Store.TaskGet(suspendedID)
    if err!=nil{t.Fatal(err)}
    if _,err=svc.TaskUpdate(suspendedID,"oauth:"+issuer+"\\x00"+regateAudience+"\\x00"+"test-user",
        update["inputResponses"].(map[string]any));err==nil {
        t.Fatal("context-free task update bypassed fresh credential")
    }
    afterDenied,err:=svc.Store.TaskGet(suspendedID)
    if err!=nil || afterDenied.Status!="input_required" {t.Fatalf("rejected resume mutated task: %#v %v",afterDenied,err)}
    _=storedTask
    fresh:=signedTestJWT(t,private,issuer,regateAudience,
        "surface:read crm:read other:scope")
    resumed:=decode(t,latestCall(t,api.URL+"/mcp/support",fresh,"tasks/update",64,update))
    if resumed["error"]!=nil {t.Fatalf("valid new bearer could not resume: %#v",resumed)}
    deadline=time.Now().Add(4*time.Second)
    resumedOK:=false
    for time.Now().Before(deadline) {
        data:=decode(t,latestCall(t,api.URL+"/mcp/support",fresh,"tasks/get",65,map[string]any{"taskId":suspendedID}))
        task:=data["result"].(map[string]any)
        if task["status"]=="completed" {
            result:=task["result"].(map[string]any)
            if result["isError"]==true {t.Fatalf("continued exchange failed: %#v",task)}
            resumedOK=true
            break
        }
        time.Sleep(10*time.Millisecond)
    }
    if !resumedOK {t.Fatal("resumed delegated execution did not complete")}
    record,err:=svc.Store.TaskGet(suspendedID)
    if err!=nil{t.Fatal(err)}
    serialized,_=json.Marshal(record)
    if strings.Contains(string(serialized),origin) || strings.Contains(string(serialized),fresh) {
        t.Fatal("resumed bearer persisted in task state")
    }
}
