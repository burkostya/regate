package identity

import (
    "context"
    "crypto/rand"
    "crypto/rsa"
    "encoding/json"
    "fmt"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
)

func TestTokenExchangeVerifiesTargetAudienceAndScopes(t *testing.T) {
    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil {t.Fatal(err)}
    t.Setenv("REGATE_EXCHANGE_TEST_ID","regate-client")
    t.Setenv("REGATE_EXCHANGE_TEST_SECRET","super-secret")
    var variant atomic.Value
    variant.Store("valid")
    var hits atomic.Int64
    const issuer="https://issuer.test"
    const sourceAudience="https://regate.test/mcp/support"
    const targetAudience="https://crm.test/resource"
    service:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
        switch r.URL.Path {
        case "/jwks":
            w.Header().Set("Content-Type","application/json")
            w.Write(privateJWKS(t,private))
        case "/token":
            hits.Add(1)
            user,password,ok:=r.BasicAuth()
            if !ok || user!="regate-client" || password!="super-secret" ||
                r.Method!=http.MethodPost {http.Error(w,"not authorized",401);return}
            if err:=r.ParseForm();err!=nil {http.Error(w,"bad body",400);return}
            if r.Form.Get("grant_type")!=tokenExchangeGrant ||
                r.Form.Get("subject_token_type")!=accessTokenType ||
                r.Form.Get("audience")!=targetAudience ||
                r.Form.Get("scope")!="crm:read" {
                http.Error(w,"bad delegation request",400)
                return
            }
            target:=targetAudience
            subject:="owner-123"
            scopes:="crm:read"
            switch variant.Load().(string) {
            case "wrong-aud":target="https://wrong.test"
            case "wrong-sub":subject="unauthorized-other-user"
            case "wrong-scope":scopes="not:crm"
            case "error":http.Error(w,"not allowed",403);return
            }
            expires:=time.Now().Add(time.Hour).Unix()
            token:=signedRS256(t,private,"primary",map[string]any{
                "iss":issuer,"aud":target,"sub":subject,"exp":expires,"scope":scopes,
            })
            switch variant.Load().(string) {
            case "duplicate-token":
                fmt.Fprintf(w,`{"access_token":"forged","access_token":%q,"issued_token_type":%q,"token_type":"Bearer","expires_in":3600}`,token,accessTokenType)
                return
            case "duplicate-nested":
                fmt.Fprintf(w,`{"access_token":%q,"issued_token_type":%q,"token_type":"Bearer","expires_in":3600,"policy":{"active":false,"active":true}}`,token,accessTokenType)
                return
            }
            json.NewEncoder(w).Encode(map[string]any{
                "access_token":token,"issued_token_type":accessTokenType,
                "token_type":"Bearer","expires_in":3600,
            })
        default:
            http.NotFound(w,r)
        }
    }))
    defer service.Close()
    provider:=config.OAuth{Name:"company",Issuer:issuer,Audience:sourceAudience,JWKSURL:service.URL+"/jwks",
        TokenEndpoint:service.URL+"/token",
        ExchangeClientIDEnv:"REGATE_EXCHANGE_TEST_ID",ExchangeClientSecretEnv:"REGATE_EXCHANGE_TEST_SECRET"}
    source:=signedRS256(t,private,"primary",map[string]any{
        "iss":issuer,"aud":sourceAudience,"sub":"owner-123",
        "exp":time.Now().Add(time.Hour).Unix(),"scope":"surface:read",
    })
    principal,err:=NewVerifier(provider).Verify(context.Background(),source)
    if err!=nil {t.Fatal(err)}
    ctx:=WithSubjectToken(WithPrincipal(context.Background(),principal),source)
    target,err:=Exchange(ctx,provider,targetAudience,[]string{"crm:read"})
    if err!=nil || target==source || strings.Count(target,".")!=2 {
        t.Fatalf("token exchange did not issue distinct audience credential: %v",err)
    }
    if hits.Load()!=1 {t.Fatalf("exchange endpoint not used: %d",hits.Load())}
    for _,test:=range []string{"wrong-aud","wrong-sub","wrong-scope","error","duplicate-token","duplicate-nested"} {
        t.Run(test,func(t *testing.T){
            variant.Store(test)
            if _,err:=Exchange(ctx,provider,targetAudience,[]string{"crm:read"});err!=ErrTokenExchange {
                t.Fatalf("accepted invalid delegated token %q: %v",test,err)
            }
        })
    }
    if _,err:=Exchange(context.Background(),provider,targetAudience,[]string{"crm:read"});err!=ErrTokenExchange {
        t.Fatalf("accepted exchange without a verified source token: %v",err)
    }
    t.Setenv("REGATE_EXCHANGE_TEST_SECRET","")
    if _,err:=Exchange(ctx,provider,targetAudience,[]string{"crm:read"});err!=ErrTokenExchange {
        t.Fatalf("accepted exchange without client credentials: %v",err)
    }
}
