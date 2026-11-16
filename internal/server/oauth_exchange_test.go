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
    // An async job cannot durably reuse a transient subject bearer token.
    async:=map[string]any{"name":"get_customer_direct","arguments":map[string]any{"customer_id":"C1"},
        "_meta":map[string]any{
            "io.modelcontextprotocol/protocolVersion":"2026-07-28",
            "io.modelcontextprotocol/clientCapabilities":map[string]any{
                "extensions":map[string]any{"io.modelcontextprotocol/tasks":map[string]any{}},
            },
        },
    }
    start:=decode(t,latestCall(t,api.URL+"/mcp/support",origin,"tools/call",53,async))
    if start["error"]==nil || start["result"]!=nil {
        t.Fatalf("insecure async subject token persistence was allowed: %#v",start)
    }
}
