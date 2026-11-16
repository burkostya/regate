package server

import (
    "bytes"
    "context"
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
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/storage"
)

func TestOAuthTaskGrantProvisionOwnerBindingAndRevocation(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","admin-test-secret")
    t.Setenv("REGATE_OAUTH_EXCHANGE_ID","regate-client")
    t.Setenv("REGATE_OAUTH_EXCHANGE_SECRET","exchange-secret")
    key:=base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{77},32))
    ring,_:=json.Marshal(map[string]any{"active":"k1","keys":map[string]string{"k1":key}})
    t.Setenv("REGATE_GRANT_KEYS_JSON",string(ring))

    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil{t.Fatal(err)}
    const issuer="https://issuer.test"
    const audience="https://regate.test/mcp/support"
    var mode atomic.Value
    mode.Store("valid")
    var exchanges atomic.Int64
    var upstreamExchanges atomic.Int64
    idp:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        switch r.URL.Path {
        case "/jwks":
            json.NewEncoder(w).Encode(map[string]any{"keys":[]any{map[string]any{
                "kid":"test-key","kty":"RSA","alg":"RS256","use":"sig",
                "n":base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
                "e":base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.PublicKey.E)).Bytes()),
            }}})
        case "/token":
            name,secret,valid:=r.BasicAuth()
            if !valid || name!="regate-client" || secret!="exchange-secret" ||
                r.ParseForm()!=nil {
                http.Error(w,"bad OAuth client",401);return
            }
            switch r.Form.Get("grant_type") {
            case "refresh_token":
                exchanges.Add(1)
                refresh:=r.Form.Get("refresh_token")
                if r.Form.Get("scope")!="surface:read crm:write" ||
                    (refresh!="supplied-once" && refresh!="rotated-sensitive-refresh") {
                    http.Error(w,"bad refresh request",401);return
                }
                subject:="test-user"
                if mode.Load().(string)=="wrong-owner" {subject="different-user"}
                access:=signedTestJWTSubject(t,private,issuer,audience,
                    "surface:read crm:write",subject)
                rotated:="rotated-sensitive-refresh"
                if refresh=="rotated-sensitive-refresh" {rotated="rotated-final-refresh"}
                json.NewEncoder(w).Encode(map[string]any{
                    "access_token":access,"refresh_token":rotated,
                    "token_type":"Bearer","expires_in":3600,
                    "refresh_token_expires_in":86400,
                    "scope":"surface:read crm:write",
                })
            case "urn:ietf:params:oauth:grant-type:token-exchange":
                upstreamExchanges.Add(1)
                if r.Form.Get("audience")!="https://crm.test/resource" ||
                    r.Form.Get("scope")!="crm:write" ||
                    r.Form.Get("subject_token_type")!="urn:ietf:params:oauth:token-type:access_token" {
                    http.Error(w,"bad token exchange",401);return
                }
                delegated:=signedTestJWTSubject(t,private,issuer,
                    "https://crm.test/resource","crm:write","test-user")
                json.NewEncoder(w).Encode(map[string]any{
                    "access_token":delegated,"token_type":"Bearer",
                    "issued_token_type":"urn:ietf:params:oauth:token-type:access_token",
                    "expires_in":3600,
                })
            default:
                http.Error(w,"unexpected grant type",400)
            }
        default:
            http.NotFound(w,r)
        }
    }))
    defer idp.Close()
    world:=&mockWorld{}
    backend:=httptest.NewServer(world)
    defer backend.Close()

    source,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil{t.Fatal(err)}
    hcl:=strings.ReplaceAll(string(source),"http://127.0.0.1:9091",backend.URL)
    hcl=strings.Replace(hcl,`upstream "crm" {`,
        `upstream "crm" {
          trust_level = "restricted"
          oauth_exchange = "company"
          oauth_audience = "https://crm.test/resource"
          oauth_scopes = ["crm:write"]`,1)
    hcl=strings.Replace(hcl,`surface "support" {
  path = "/mcp/support"`,`surface "support" {
  path = "/mcp/support"
  oauth = "company"
  required_scopes = ["surface:read"]`,1)
    hcl=strings.Replace(hcl,`  tool "get_customer_direct" {`,`
  tool "deferred_customer" {
    pipeline = "deferred_customer"
    required_scopes = ["crm:write"]
    data_classification = "restricted"
  }
  tool "get_customer_direct" {`,1)
    hcl+=`
pipeline "deferred_customer" {
  entry = "pause"
  node "pause" {
    kind = "suspend"
  }
  node "fetch" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "get_customer"
    effect = "read"
    arguments = {customer_id = "input.customer_id"}
  }
  node "result" {
    kind = "output"
    value = "nodes.fetch.value"
  }
  edge {
    from = "pause"
    to = "fetch"
  }
  edge {
    from = "fetch"
    to = "result"
  }
}
`
    hcl=fmt.Sprintf(`oauth "company" {
      issuer = %q
      jwks_url = %q
      audience = %q
      token_endpoint = %q
      exchange_client_id_env = "REGATE_OAUTH_EXCHANGE_ID"
      exchange_client_secret_env = "REGATE_OAUTH_EXCHANGE_SECRET"
    }
    `,issuer,idp.URL+"/jwks",audience,idp.URL+"/token")+hcl
    svc,err:=New(t.TempDir(),hcl)
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()
    subjectBearer:=signedTestJWT(t,private,issuer,audience,"surface:read crm:write")
    asyncParams:=map[string]any{
        "name":"deferred_customer",
        "arguments":map[string]any{"customer_id":"C123"},
        "_meta":map[string]any{
            "io.modelcontextprotocol/protocolVersion":"2026-07-28",
            "io.modelcontextprotocol/clientCapabilities":map[string]any{
                "extensions":map[string]any{"io.modelcontextprotocol/tasks":map[string]any{}},
            },
        },
    }
    startTask:=func()string {
        t.Helper()
        result:=decode(t,latestCall(t,api.URL+"/mcp/support",
            subjectBearer,"tools/call",101,asyncParams))
        if result["error"]!=nil{t.Fatalf("cannot start delegated task: %#v",result)}
        id,ok:=result["result"].(map[string]any)["taskId"].(string)
        if !ok {t.Fatalf("missing durable task ID: %#v",result)}
        deadline:=time.Now().Add(4*time.Second)
        for time.Now().Before(deadline) {
            poll:=decode(t,latestCall(t,api.URL+"/mcp/support",subjectBearer,
                "tasks/get",102,map[string]any{"taskId":id}))
            if poll["result"].(map[string]any)["status"]=="input_required" {return id}
            time.Sleep(10*time.Millisecond)
        }
        t.Fatal("delegated task did not suspend")
        return ""
    }
    taskID:=startTask()
    params:=map[string]any{"taskId":taskID,"refreshToken":"supplied-once"}
    other:=signedTestJWTSubject(t,private,issuer,audience,
        "surface:read crm:write","other-user")
    unauthorized:=latestCall(t,api.URL+"/mcp/support",other,"tasks/grants/provision",103,params)
    unauthorized.Body.Close()
    if unauthorized.StatusCode<400 {t.Fatal("different task owner provisioned grant")}
    limited:=signedTestJWT(t,private,issuer,audience,"surface:read")
    unauthorized=latestCall(t,api.URL+"/mcp/support",limited,"tasks/grants/provision",104,params)
    unauthorized.Body.Close()
    if unauthorized.StatusCode<400 {t.Fatal("insufficient scopes provisioned grant")}
    if exchanges.Load()!=0 {t.Fatal("untrusted caller reached OAuth token endpoint")}

    active:=decode(t,latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/provision",105,params))
    if active["error"]!=nil{t.Fatalf("verified grant provision rejected: %#v",active)}
    status:=active["result"].(map[string]any)
    if status["state"]!="active" {t.Fatalf("grant not activated: %#v",status)}
    if exchanges.Load()!=1 {t.Fatalf("unexpected OAuth refresh count %d",exchanges.Load())}
    repeated:=latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/provision",106,params)
    repeated.Body.Close()
    if repeated.StatusCode<400 {t.Fatal("same task grant provision was repeated")}
    if exchanges.Load()!=1 {t.Fatal("duplicate token refresh call after provision")}

    read:=decode(t,latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/status",107,map[string]any{"taskId":taskID}))
    if read["result"].(map[string]any)["state"]!="active"{
        t.Fatalf("grant public state not visible to owner: %#v",read)
    }
    ownerKey:="oauth:"+issuer+"\x00"+audience+"\x00test-user"
    record,err:=svc.Store.TaskGet(taskID)
    if err!=nil{t.Fatal(err)}
    if record.OwnerDigest!=fingerprint(ownerKey){t.Fatal("wrong principal was bound to grant")}
    persisted,err:=svc.Store.TaskGrantState(context.Background(),taskID,record.OwnerDigest)
    if err!=nil || persisted.State!="active" {t.Fatalf("sealed state unavailable: %#v %v",persisted,err)}
    raw,_:=json.Marshal(record.Public())
    if bytes.Contains(raw,[]byte("rotated-sensitive-refresh")) ||
        bytes.Contains(raw,[]byte("supplied-once")) {t.Fatal("refresh token disclosed in task response")}
    db,err:=os.ReadFile(svc.Store.Dir+"/regate.sqlite")
    if err!=nil{t.Fatal(err)}
    if bytes.Contains(db,[]byte("rotated-sensitive-refresh")) ||
        bytes.Contains(db,[]byte("supplied-once")) {t.Fatal("refresh token written in SQLite plaintext")}

    // Admin can explicitly resume through the sealed task grant but cannot
    // reuse the admin bearer as an upstream OAuth subject token.
    unauthorizedAdmin:=jsonPost(t,api.URL+"/api/task-resume-grant","",
        map[string]any{"task_id":taskID,"value":map[string]any{"approval":true}})
    unauthorizedAdmin.Body.Close()
    if unauthorizedAdmin.StatusCode!=http.StatusUnauthorized {
        t.Fatal("anonymous grant resume permitted")
    }
    begin:=decode(t,jsonPost(t,api.URL+"/api/task-resume-grant","admin-test-secret",
        map[string]any{"task_id":taskID,"value":map[string]any{"approval":true}}))
    if begin["status"]!="working" {t.Fatalf("grant continuation not admitted: %#v",begin)}
    if exchanges.Load()!=2 {
        t.Fatalf("unexpected refresh rotation count: %d",exchanges.Load())
    }
    deadline:=time.Now().Add(6*time.Second)
    complete:=false
    for time.Now().Before(deadline) {
        polled:=decode(t,latestCall(t,api.URL+"/mcp/support",subjectBearer,
            "tasks/get",112,map[string]any{"taskId":taskID}))
        taskState:=polled["result"].(map[string]any)
        if taskState["status"]=="completed" {
            res:=taskState["result"].(map[string]any)
            if res["isError"]==true {t.Fatalf("grant continuation failed: %#v",res)}
            complete=true
            break
        }
        time.Sleep(10*time.Millisecond)
    }
    if !complete {t.Fatal("grant continuation never completed")}
    if upstreamExchanges.Load()!=1 {
        t.Fatalf("expected one audience-specific exchange, got %d",upstreamExchanges.Load())
    }
    world.mu.Lock()
    targetAuth:=world.lastAuth
    world.mu.Unlock()
    if !strings.HasPrefix(targetAuth,"Bearer eyJ") ||
        strings.Contains(targetAuth,subjectBearer) {
        t.Fatal("Regate subject token leaked or missing audience-bound exchange")
    }
    replay:=jsonPost(t,api.URL+"/api/task-resume-grant","admin-test-secret",
        map[string]any{"task_id":taskID,"value":map[string]any{"approval":true}})
    replay.Body.Close()
    if replay.StatusCode!=http.StatusBadRequest || exchanges.Load()!=2 {
        t.Fatal("same checkpoint reexecuted with a refreshed grant")
    }

    revoked:=decode(t,latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/revoke",108,map[string]any{"taskId":taskID}))
    if revoked["result"].(map[string]any)["state"]!="revoked" {t.Fatal("revoke did not acknowledge")}
    if _,_,err=svc.Store.BeginTaskGrantRefresh(context.Background(),taskID,record.OwnerDigest);
        err!=storage.ErrGrantUnavailable && err!=storage.ErrGrantRefreshPending {
        t.Fatalf("revoked grant reopened: %v",err)
    }
    if exchanges.Load()!=2 {t.Fatal("revocation triggered an unexpected token request")}

    // A resource server must not expose delegated credential provisioning on
    // the management bearer path, regardless of its administrator privileges.
    admin:=latestCall(t,api.URL+"/mcp/admin","admin-test-secret",
        "tasks/grants/provision",109,params)
    defer admin.Body.Close()
    var adminResult map[string]any
    _=json.NewDecoder(admin.Body).Decode(&adminResult)
    if admin.StatusCode==200 && adminResult["error"]==nil {
        t.Fatal("admin bearer gained owner-scoped refresh capability")
    }

    // A failed refresh for a second task stays indeterminate; the provider is
    // never contacted again by a repeated provision request.
    failedID:=startTask()
    mode.Store("wrong-owner")
    badParams:=map[string]any{"taskId":failedID,"refreshToken":"supplied-once"}
    invalid:=latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/provision",110,badParams)
    invalid.Body.Close()
    if invalid.StatusCode<400 {t.Fatal("wrong-owner refreshed token was accepted")}
    before:=exchanges.Load()
    again:=latestCall(t,api.URL+"/mcp/support",subjectBearer,
        "tasks/grants/provision",111,badParams)
    again.Body.Close()
    if again.StatusCode<400 {t.Fatal("repeated ambiguous OAuth grant accepted")}
    if exchanges.Load()!=before {t.Fatal("indeterminate external token refresh retried")}
    task,err:=svc.Store.TaskGet(failedID)
    if err!=nil{t.Fatal(err)}
    uncertain,err:=svc.Store.TaskGrantState(context.Background(),failedID,task.OwnerDigest)
    if err!=nil || uncertain.State!="needs_reauth" {
        t.Fatalf("failed OAuth credential remained live: %#v %v",uncertain,err)
    }
}
