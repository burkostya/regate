package identity

import (
    "context"
    "crypto/rand"
    "crypto/rsa"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync/atomic"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/config"
)

func TestOAuthRefreshGrantVerifiesIssuerOwnerAudienceAndScopes(t *testing.T){
    private,err:=rsa.GenerateKey(rand.Reader,2048)
    if err!=nil{t.Fatal(err)}
    t.Setenv("REGATE_REFRESH_CLIENT_ID","regate-client")
    t.Setenv("REGATE_REFRESH_CLIENT_SECRET","client-secret")
    const issuer="https://identity.test"
    const aud="https://regate.test/surface"
    var variant atomic.Value
    variant.Store("ok")
    var hits atomic.Int64
    server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        switch r.URL.Path {
        case "/jwks":
            w.Header().Set("Content-Type","application/json")
            w.Write(privateJWKS(t,private))
        case "/token":
            hits.Add(1)
            name,secret,has:=r.BasicAuth()
            if !has || name!="regate-client" || secret!="client-secret" ||
                r.ParseForm()!=nil || r.Form.Get("grant_type")!="refresh_token" ||
                r.Form.Get("refresh_token")!="original-refresh" ||
                r.Form.Get("scope")!="surface:read crm:write" {
                http.Error(w,"invalid refresh client",401)
                return
            }
            mode:=variant.Load().(string)
            if mode=="network-error" {
                if hijack,ok:=w.(http.Hijacker);ok {
                    conn,_,err:=hijack.Hijack()
                    if err==nil{conn.Close();return}
                }
                http.Error(w,"unavailable",502)
                return
            }
            resultAudience:=aud
            resultSubject:="owner-123"
            resultScopes:="surface:read crm:write"
            refreshExpires:=int64(3600)
            if mode=="other-audience" {resultAudience="https://someone-else.test"}
            if mode=="other-subject" {resultSubject="other-user"}
            if mode=="narrowed-scopes" {resultScopes="surface:read"}
            if mode=="expired-refresh" {refreshExpires=0}
            access:=signedRS256(t,private,"primary",map[string]any{
                "iss":issuer,"aud":resultAudience,"sub":resultSubject,
                "exp":time.Now().Add(time.Hour).Unix(),"scope":resultScopes,
            })
            json.NewEncoder(w).Encode(map[string]any{
                "access_token":access,"refresh_token":"rotated-refresh",
                "token_type":"Bearer","expires_in":3600,
                "refresh_token_expires_in":refreshExpires,
                "scope":resultScopes,
            })
        default:
            http.NotFound(w,r)
        }
    }))
    defer server.Close()
    cfg:=config.OAuth{
        Name:"corp",Issuer:issuer,Audience:aud,
        JWKSURL:server.URL+"/jwks",TokenEndpoint:server.URL+"/token",
        ExchangeClientIDEnv:"REGATE_REFRESH_CLIENT_ID",
        ExchangeClientSecretEnv:"REGATE_REFRESH_CLIENT_SECRET",
    }
    fingerprint:=sha256.Sum256([]byte("oauth:"+issuer+"\x00"+aud+"\x00"+"owner-123"))
    owner:=hex.EncodeToString(fingerprint[:])
    required:=[]string{"surface:read","crm:write"}
    success,err:=RefreshTaskGrant(context.Background(),cfg,"original-refresh",owner,required)
    if err!=nil || success.AccessToken=="" || success.RefreshToken!="rotated-refresh" ||
        success.Principal.Subject!="owner-123" ||
        time.Until(success.RefreshExpiresAt)<time.Hour-10*time.Second {
        t.Fatalf("verified refresh rejected: %v %#v",err,success.Principal)
    }
    for _,mode:=range []string{"other-audience","other-subject","narrowed-scopes","expired-refresh","network-error"}{
        t.Run(mode,func(t *testing.T){
            variant.Store(mode)
            if _,err:=RefreshTaskGrant(context.Background(),cfg,"original-refresh",owner,required);
                err!=ErrTokenExchange {
                t.Fatalf("invalid refresh accepted: %v",err)
            }
        })
    }
    variant.Store("ok")
    if _,err:=RefreshTaskGrant(context.Background(),cfg,"original-refresh",
        strings.Repeat("a",64),required);err!=ErrTokenExchange {
        t.Fatalf("wrong bound owner accepted: %v",err)
    }
    if _,err:=RefreshTaskGrant(context.Background(),cfg,"",owner,required);
        err!=ErrTokenExchange {t.Fatalf("empty refresh token accepted: %v",err)}
    if hits.Load()!=7 {t.Fatalf("unexpected refresh request count: %d",hits.Load())}
}
