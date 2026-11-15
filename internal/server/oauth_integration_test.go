package server

import (
    "bytes"
    "crypto"
    "crypto/rand"
    "crypto/rsa"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "io"
    "math/big"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "sync"
    "testing"
    "time"
)

func jwtJSON(v any) string {
    data,_:=json.Marshal(v)
    return base64.RawURLEncoding.EncodeToString(data)
}
func signedTestJWT(t *testing.T,private *rsa.PrivateKey,issuer,audience,scopes string) string {
    t.Helper()
    claims:=map[string]any{
        "iss":issuer,"aud":audience,"sub":"test-user","exp":time.Now().Add(time.Hour).Unix(),"scope":scopes,
    }
    unsigned:=jwtJSON(map[string]any{"alg":"RS256","kid":"test-key"})+"."+jwtJSON(claims)
    h:=sha256.Sum256([]byte(unsigned))
    sig,err:=rsa.SignPKCS1v15(rand.Reader,private,crypto.SHA256,h[:])
    if err!=nil {t.Fatal(err)}
    return unsigned+"."+base64.RawURLEncoding.EncodeToString(sig)
}
func oauthFixture(t *testing.T) (*Service,*httptest.Server,*mockWorld,*rsa.PrivateKey) {
    t.Helper()
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    world:=&mockWorld{}
    crm:=httptest.NewServer(world)
    t.Cleanup(crm.Close)
    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil {t.Fatal(err)}
    keyServer:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        if r.URL.Path!="/jwks" {http.NotFound(w,r);return}
        key:=map[string]any{
            "kid":"test-key","kty":"RSA","alg":"RS256","use":"sig",
            "n":base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
            "e":base64.RawURLEncoding.EncodeToString(big.NewInt(int64(private.PublicKey.E)).Bytes()),
        }
        json.NewEncoder(w).Encode(map[string]any{"keys":[]any{key}})
    }))
    t.Cleanup(keyServer.Close)
    b,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    src:=strings.ReplaceAll(string(b),"http://127.0.0.1:9091",crm.URL)
    src=fmt.Sprintf(`oauth "company" {
      issuer = "https://issuer.test"
      jwks_url = %q
      audience = "https://regate.test/mcp/support"
    }
    `,keyServer.URL+"/jwks")+src
    src=strings.Replace(src,`surface "support" {
  path = "/mcp/support"`,`surface "support" {
  path = "/mcp/support"
  oauth = "company"
  required_scopes = ["surface:read"]`,1)
    src=strings.Replace(src,`  tool "summarize_customer" {`,`  tool "summarize_customer" {
    required_scopes = ["crm:read"]
    data_classification = "restricted"`,1)
    src=strings.Replace(src,`  tool "get_customer_direct" {`,`  tool "get_customer_direct" {
    required_scopes = ["crm:read"]
    data_classification = "restricted"`,1)
    src=strings.Replace(src,`  resource "instructions" {`,`  resource "instructions" {
    required_scopes = ["docs:read"]`,1)
    src=strings.Replace(src,`  prompt "customer_review" {`,`  prompt "customer_review" {
    required_scopes = ["docs:read"]`,1)
    svc,err:=New(t.TempDir(),src)
    if err!=nil {t.Fatalf("OAuth configuration invalid: %v",err)}
    srv:=httptest.NewServer(svc.Handler())
    t.Cleanup(srv.Close)
    return svc,srv,world,private
}

func TestOAuthSurfaceRejectsWrongIssuerAudienceAndAnonymous(t *testing.T) {
    svc,srv,world,private:=oauthFixture(t)
    _=svc
    cases:=[]struct{name,token string;want int}{
        {"missing-token","",http.StatusUnauthorized},
        {"legacy-admin-token","test-secret",http.StatusUnauthorized},
        {"wrong-issuer",signedTestJWT(t,private,"https://wrong.test","https://regate.test/mcp/support","surface:read"),http.StatusUnauthorized},
        {"wrong-audience",signedTestJWT(t,private,"https://issuer.test","https://other.test/mcp","surface:read"),http.StatusUnauthorized},
        {"missing-surface-scope",signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","crm:read"),http.StatusForbidden},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T) {
            req,err:=http.NewRequest(http.MethodPost,srv.URL+"/mcp/support",strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
            if err!=nil {t.Fatal(err)}
            req.Header.Set("Content-Type","application/json")
            if tc.token!="" {req.Header.Set("Authorization","Bearer "+tc.token)}
            resp,err:=http.DefaultClient.Do(req)
            if err!=nil {t.Fatal(err)}
            defer resp.Body.Close()
            if resp.StatusCode!=tc.want {body,_:=io.ReadAll(resp.Body);t.Fatalf("HTTP %d want %d: %s",resp.StatusCode,tc.want,body)}
        })
    }
    world.mu.Lock()
    defer world.mu.Unlock()
    if len(world.calls)!=0 {t.Fatalf("rejected access reached downstream: %v",world.calls)}
}

func TestOAuthScopesFilterDiscoveryAndPreventUnauthorizedEffects(t *testing.T) {
    _,srv,world,private:=oauthFixture(t)
    token:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read")
    tools:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"tools/list",2,nil))
    if list:=tools["result"].(map[string]any)["tools"].([]any);len(list)!=0 {
        t.Fatalf("tools lacking scopes were enumerated: %v",list)
    }
    resources:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"resources/list",3,nil))
    if list:=resources["result"].(map[string]any)["resources"].([]any);len(list)!=0 {
        t.Fatalf("restricted resource URIs were enumerated: %v",list)
    }
    prompts:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"prompts/list",4,nil))
    if list:=prompts["result"].(map[string]any)["prompts"].([]any);len(list)!=0 {
        t.Fatalf("restricted prompts were enumerated: %v",list)
    }
    denied:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"tools/call",5,
        map[string]any{"name":"get_customer_direct","arguments":map[string]any{"customer_id":"C1"}}))
    if denied["result"].(map[string]any)["isError"]!=true {
        t.Fatalf("unauthorized tool call accepted: %v",denied)
    }
    world.mu.Lock()
    if len(world.calls)!=0 {t.Fatalf("MCP called without required scope: %v",world.calls)}
    world.mu.Unlock()
    broad:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read docs:read")
    list:=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"tools/list",6,nil))["result"].(map[string]any)["tools"].([]any)
    if len(list)!=2 {t.Fatalf("scoped tools missing: %v",list)}
    content:=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"tools/call",7,
        map[string]any{"name":"get_customer_direct","arguments":map[string]any{"customer_id":"C1"}}))
    output:=content["result"].(map[string]any)
    printable,_:=json.Marshal(output)
    if output["isError"]==true || bytes.Contains(printable,[]byte("alice@example.com")) {
        t.Fatalf("OAuth tool response contains unmasked PII: %s",printable)
    }
    resources=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"resources/list",8,nil))
    if len(resources["result"].(map[string]any)["resources"].([]any))!=1 {
        t.Fatal("authorized resource missing")
    }
    prompts=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"prompts/list",9,nil))
    if len(prompts["result"].(map[string]any)["prompts"].([]any))!=1 {
        t.Fatal("authorized prompt missing")
    }
}

func TestOAuthMetadataIsPublicAndAdminRemainsSeparate(t *testing.T) {
    _,srv,_,private:=oauthFixture(t)
    resp,err:=http.Get(srv.URL+"/.well-known/oauth-protected-resource/mcp/support")
    if err!=nil {t.Fatal(err)}
    metadata:=decode(t,resp)
    if metadata["resource"]!="https://regate.test/mcp/support" {t.Fatalf("wrong audience: %v",metadata)}
    servers:=metadata["authorization_servers"].([]any)
    if len(servers)!=1 || servers[0]!="https://issuer.test" {t.Fatalf("wrong issuer: %v",metadata)}
    token:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read")
    req,err:=http.NewRequest(http.MethodPost,srv.URL+"/mcp/admin",strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
    if err!=nil {t.Fatal(err)}
    req.Header.Set("Content-Type","application/json")
    req.Header.Set("Authorization","Bearer "+token)
    response,err:=http.DefaultClient.Do(req)
    if err!=nil {t.Fatal(err)}
    defer response.Body.Close()
    if response.StatusCode!=http.StatusUnauthorized {t.Fatalf("public OAuth principal used as admin: HTTP %d",response.StatusCode)}
}

func TestOAuthTaskCannotBypassToolScopeAndMasking(t *testing.T) {
    svc,srv,world,private:=oauthFixture(t)
    limited:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read")
    taskParams:=map[string]any{
        "name":"get_customer_direct",
        "arguments":map[string]any{"customer_id":"C1"},
        "_meta":map[string]any{
            "io.modelcontextprotocol/protocolVersion":"2026-07-28",
            "io.modelcontextprotocol/clientCapabilities":map[string]any{"extensions":map[string]any{"io.modelcontextprotocol/tasks":map[string]any{}}},
        },
    }
    denial:=decode(t,latestCall(t,srv.URL+"/mcp/support",limited,"tools/call",21,taskParams))
    if denial["error"]==nil {t.Fatalf("task bypassed tool scopes: %v",denial)}
    world.mu.Lock()
    if len(world.calls)!=0 {t.Fatalf("unauthorized async tool executed: %v",world.calls)}
    world.mu.Unlock()
    broad:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read")
    start:=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"tools/call",22,taskParams))
    result:=start["result"].(map[string]any)
    id,ok:=result["taskId"].(string)
    if !ok {t.Fatalf("async handle absent: %v",start)}
    var completed map[string]any
    deadline:=time.Now().Add(4*time.Second)
    for time.Now().Before(deadline) {
        data:=decode(t,latestCall(t,srv.URL+"/mcp/support",broad,"tasks/get",23,map[string]any{"taskId":id}))
        if value,ok:=data["result"].(map[string]any);ok && value["status"]=="completed" {
            completed=value
            break
        }
        time.Sleep(10*time.Millisecond)
    }
    if completed==nil {t.Fatalf("task not completed")}
    printed,_:=json.Marshal(completed)
    if bytes.Contains(printed,[]byte("alice@example.com")) {t.Fatalf("async result leaked PII: %s",printed)}
    stored,err:=svc.Store.TaskGet(id)
    if err!=nil {t.Fatal(err)}
    if !stored.RedactPII || len(stored.RequiredScopes)!=1 || stored.RequiredScopes[0]!="crm:read" {
        t.Fatalf("async task lost auth/redaction contract: %#v",stored)
    }
    // A token with the same identity but a different signed JWT is not a
    // sufficient task owner; secrets are bound to the specific client token.
    other:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read other:scope")
    response:=latestCall(t,srv.URL+"/mcp/support",other,"tasks/get",24,map[string]any{"taskId":id})
    defer response.Body.Close()
    data,_:=io.ReadAll(response.Body)
    if response.StatusCode<400 || !bytes.Contains(data,[]byte(`"error"`)) {
        t.Fatalf("task available to distinct bearer: HTTP %d %s",response.StatusCode,data)
    }
}

func TestOAuthConcurrentDiscoveryAuthorization(t *testing.T) {
    _,srv,_,private:=oauthFixture(t)
    limited:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read")
    full:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read")
    var wg sync.WaitGroup
    for _,token:=range []string{limited,full} {
        token:=token
        wg.Add(1)
        go func() {
            defer wg.Done()
            for i:=0;i<4;i++ {
                req,err:=http.NewRequest(http.MethodPost,srv.URL+"/mcp/support",strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`))
                if err!=nil {t.Error(err);return}
                req.Header.Set("Content-Type","application/json")
                req.Header.Set("Authorization","Bearer "+token)
                res,err:=http.DefaultClient.Do(req)
                if err!=nil {t.Error(err);return}
                b,_:=io.ReadAll(res.Body)
                res.Body.Close()
                if res.StatusCode!=200 {t.Errorf("authorization race HTTP %d %s",res.StatusCode,b);return}
                var result map[string]any
                if json.Unmarshal(b,&result)!=nil {t.Errorf("bad response %s",b);return}
                list:=result["result"].(map[string]any)["tools"].([]any)
                want:=0
                if token==full {want=2}
                if len(list)!=want {t.Errorf("mixed principal's tool catalog: %d want %d",len(list),want);return}
            }
        }()
    }
    wg.Wait()
}
