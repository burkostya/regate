package identity

import (
    "context"
    "crypto"
    "crypto/rand"
    "crypto/rsa"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "math/big"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
)

func encoded(v any) string {
    b,_:=json.Marshal(v)
    return base64.RawURLEncoding.EncodeToString(b)
}
func signedRS256(t *testing.T,private *rsa.PrivateKey,kid string,claims map[string]any)string {
    t.Helper()
    body:=encoded(map[string]any{"alg":"RS256","kid":kid,"typ":"JWT"})+"."+encoded(claims)
    h:=sha256.Sum256([]byte(body))
    sig,err:=rsa.SignPKCS1v15(rand.Reader,private,crypto.SHA256,h[:])
    if err!=nil {t.Fatal(err)}
    return body+"."+base64.RawURLEncoding.EncodeToString(sig)
}
func privateJWKS(t *testing.T,private *rsa.PrivateKey) []byte {
    t.Helper()
    e:=big.NewInt(int64(private.PublicKey.E))
    payload,_:=json.Marshal(map[string]any{"keys":[]any{map[string]any{
        "kid":"primary","kty":"RSA","alg":"RS256","use":"sig",
        "n":base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()),
        "e":base64.RawURLEncoding.EncodeToString(e.Bytes()),
    }}})
    return payload
}
func TestJWTIssuerAudienceExpiryScopeAndSignature(t *testing.T) {
    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil {t.Fatal(err)}
    var keyRequests atomic.Int64
    jwks:=privateJWKS(t,private)
    endpoint:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        keyRequests.Add(1)
        w.Header().Set("Content-Type","application/json")
        w.Write(jwks)
    }))
    defer endpoint.Close()
    cfg:=config.OAuth{Issuer:"https://issuer.test", Audience:"https://regate.test/mcp/private",JWKSURL:endpoint.URL}
    verifier:=NewVerifier(cfg)
    exp:=time.Now().Add(time.Hour).Unix()
    base:=map[string]any{
        "iss":cfg.Issuer,"aud":[]string{"https://other.test",cfg.Audience},
        "sub":"user-42","exp":exp,"scope":"surface:read crm:read",
    }
    token:=signedRS256(t,private,"primary",base)
    principal,err:=verifier.Verify(context.Background(),token)
    if err!=nil {t.Fatalf("valid RS256 rejected: %v",err)}
    if !principal.HasAll([]string{"surface:read","crm:read"}) || principal.HasAll([]string{"admin:write"}) {
        t.Fatalf("incorrect scopes: %#v",principal.Scopes)
    }
    if got:=keyRequests.Load();got!=1 {t.Fatalf("JWKS cache absent: %d",got)}
    if _,err:=verifier.Verify(context.Background(),token);err!=nil {t.Fatal(err)}
    if got:=keyRequests.Load();got!=1 {t.Fatalf("JWKS reloaded unnecessarily: %d",got)}
    bad:=[]struct{name string;modify func(map[string]any)}{
        {"wrong-issuer",func(c map[string]any){c["iss"]="https://attacker.test"}},
        {"wrong-audience",func(c map[string]any){c["aud"]="https://other.test"}},
        {"expired",func(c map[string]any){c["exp"]=time.Now().Add(-time.Second).Unix()}},
        {"no-exp",func(c map[string]any){delete(c,"exp")}},
        {"no-sub",func(c map[string]any){delete(c,"sub")}},
        {"future-nbf",func(c map[string]any){c["nbf"]=time.Now().Add(time.Hour).Unix()}},
        {"future-iat",func(c map[string]any){c["iat"]=time.Now().Add(time.Hour).Unix()}},
    }
    for _,tc:=range bad {
        t.Run(tc.name,func(t *testing.T) {
            claims:=map[string]any{}
            for k,v:=range base {claims[k]=v}
            tc.modify(claims)
            _,err:=verifier.Verify(context.Background(),signedRS256(t,private,"primary",claims))
            if !errorsIsInvalid(err) {t.Fatalf("accepted invalid claims: %v",err)}
        })
    }
    other,_:=rsa.GenerateKey(rand.Reader,2048)
    forged:=signedRS256(t,other,"primary",base)
    if _,err:=verifier.Verify(context.Background(),forged);!errorsIsInvalid(err) {t.Fatalf("accepted forged signature: %v",err)}
    unsigned:=encoded(map[string]any{"kid":"primary","alg":"none"})+"."+encoded(base)+"."
    if _,err:=verifier.Verify(context.Background(),unsigned);!errorsIsInvalid(err) {t.Fatalf("accepted unsigned JWT: %v",err)}
    if _,err:=verifier.Verify(context.Background(),strings.Repeat("x",16385));!errorsIsInvalid(err) {t.Fatalf("accepted unbounded token: %v",err)}
}
func errorsIsInvalid(err error) bool {return err==ErrInvalidToken}

func TestOpaqueIntrospectionRequiresTrustedIssuerAudienceAndActive(t *testing.T) {
    t.Setenv("REGATE_TEST_OAUTH_CLIENT","client-id")
    t.Setenv("REGATE_TEST_OAUTH_SECRET","client-secret")
    expiry:=time.Now().Add(time.Hour).Unix()
    var received atomic.Int64
    introspection:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        received.Add(1)
        username,password,ok:=r.BasicAuth()
        if !ok || username!="client-id" || password!="client-secret" || r.Method!="POST" {
            http.Error(w,"forbidden",403)
            return
        }
        if err:=r.ParseForm();err!=nil {http.Error(w,"bad form",400);return}
        id:=r.Form.Get("token")
        reply:=map[string]any{"active":true,"iss":"https://issuer.test","aud":"regate-audience","sub":"user-1","exp":expiry,"scope":"docs:read"}
        switch id {
        case "revoked":reply["active"]=false
        case "wrong-aud":reply["aud"]="other-audience"
        case "wrong-issuer":reply["iss"]="https://attacker.test"
        case "expired":reply["exp"]=time.Now().Add(-time.Second).Unix()
        case "malformed":w.Write([]byte("{bad"));return
        }
        w.Header().Set("Content-Type","application/json")
        json.NewEncoder(w).Encode(reply)
    }))
    defer introspection.Close()
    verifier:=NewVerifier(config.OAuth{Issuer:"https://issuer.test",Audience:"regate-audience",
        IntrospectionURL:introspection.URL,ClientIDEnv:"REGATE_TEST_OAUTH_CLIENT",ClientSecretEnv:"REGATE_TEST_OAUTH_SECRET"})
    principal,err:=verifier.Verify(context.Background(),"valid-opaque-token")
    if err!=nil || !principal.HasAll([]string{"docs:read"}) {t.Fatalf("active token denied: %v %#v",err,principal)}
    for _,token:=range []string{"revoked","wrong-aud","wrong-issuer","expired","malformed"} {
        if _,err:=verifier.Verify(context.Background(),token);err==nil {
            t.Errorf("accepted invalid introspection for %s",token)
        }
    }
    if received.Load()!=6 {t.Fatalf("token lookup bypassed: %d",received.Load())}
    t.Setenv("REGATE_TEST_OAUTH_SECRET","")
    if _,err:=verifier.Verify(context.Background(),"valid-opaque-token");err==nil {t.Fatal("missing introspection credentials accepted")}
}
func TestOAuthPrincipalContextIsScopeBound(t *testing.T) {
    ctx:=WithPrincipal(context.Background(),Principal{Subject:"u",Scopes:map[string]bool{"a":true}})
    if err:=RequireScopes(ctx,[]string{"a"});err!=nil {t.Fatal(err)}
    if err:=RequireScopes(ctx,[]string{"a","b"});err!=ErrInsufficientScope {t.Fatalf("unexpected authorization: %v",err)}
    if p,ok:=FromContext(ctx);!ok || p.Subject!="u" {t.Fatalf("principal missing: %v %v",p,ok)}
    if err:=RequireScopes(context.Background(),[]string{"a"});err!=ErrInsufficientScope {t.Fatalf("anonymous accepted: %v",err)}
    if fmt.Sprint(ErrInvalidToken)!= "invalid OAuth access token" {t.Fatal("token error changed")}
}
