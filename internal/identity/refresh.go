package identity

import (
    "context"
    "crypto/sha256"
    "crypto/subtle"
    "encoding/hex"
    "encoding/json"
    "io"
    "net/http"
    "net/url"
    "os"
    "strings"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/netguard"
)

// RefreshedTaskGrant is ephemeral, never JSON-serialized or persisted by the
// identity package. A caller must durably rotate RefreshToken before using
// AccessToken to perform a delegated remote effect.
type RefreshedTaskGrant struct {
    AccessToken string
    RefreshToken string
    Principal Principal
    RefreshExpiresAt time.Time
}
const refreshGrantType = "refresh_token"
const maxRefreshAge = 30*24*time.Hour

// RefreshTaskGrant consumes one explicitly obtained refresh credential through
// a configured, guarded OAuth token endpoint. The exchanged credential must
// be valid for the ORIGINAL Regate audience, original owner and ALL task
// scopes. This is deliberately separate from an RFC8693 upstream exchange.
//
// The operation is potentially non-idempotent. Callers MUST reserve durable
// 'refreshing' state before invocation and MUST NOT retry a lost response.
func RefreshTaskGrant(ctx context.Context,provider config.OAuth,
    refreshToken,ownerDigest string,requiredScopes []string)(RefreshedTaskGrant,error) {
    if refreshToken=="" || len(refreshToken)>8192 ||
        strings.ContainsAny(refreshToken," \n\t\r") ||
        len(ownerDigest)!=64 || provider.TokenEndpoint=="" ||
        provider.Issuer=="" || provider.Audience=="" ||
        len(requiredScopes)==0 {
        return RefreshedTaskGrant{},ErrTokenExchange
    }
    if err:=netguard.ValidateURL(provider.TokenEndpoint);err!=nil {
        return RefreshedTaskGrant{},ErrTokenExchange
    }
    clientID:=os.Getenv(provider.ExchangeClientIDEnv)
    clientSecret:=os.Getenv(provider.ExchangeClientSecretEnv)
    if clientID=="" || clientSecret=="" {return RefreshedTaskGrant{},ErrTokenExchange}

    form:=url.Values{
        "grant_type":{refreshGrantType},
        "refresh_token":{refreshToken},
        "scope":{strings.Join(requiredScopes," ")},
    }
    request,err:=http.NewRequestWithContext(ctx,http.MethodPost,
        provider.TokenEndpoint,strings.NewReader(form.Encode()))
    if err!=nil{return RefreshedTaskGrant{},ErrTokenExchange}
    request.Header.Set("Content-Type","application/x-www-form-urlencoded")
    request.SetBasicAuth(clientID,clientSecret)
    response,err:=netguard.Client(6*time.Second).Do(request)
    if err!=nil{return RefreshedTaskGrant{},ErrTokenExchange}
    defer response.Body.Close()
    if response.StatusCode!=http.StatusOK {return RefreshedTaskGrant{},ErrTokenExchange}
    body,err:=io.ReadAll(io.LimitReader(response.Body,65537))
    if err!=nil || len(body)>65536{return RefreshedTaskGrant{},ErrTokenExchange}
    claims,err:=decodeJSONObject(body)
    if err!=nil{return RefreshedTaskGrant{},ErrTokenExchange}
    bearer,_:=claims["access_token"].(string)
    rotated,_:=claims["refresh_token"].(string)
    kind,_:=claims["token_type"].(string)
    accessSeconds,accessOK:=claimTimestamp(claims["expires_in"])
    refreshSeconds,refreshOK:=claimTimestamp(claims["refresh_token_expires_in"])
    if !accessOK || !refreshOK ||
        accessSeconds<=0 || accessSeconds>86400 ||
        refreshSeconds<=30 || refreshSeconds>int64(maxRefreshAge.Seconds()) ||
        kind!="Bearer" || bearer=="" || bearer==refreshToken ||
        rotated=="" || len(rotated)>8192 ||
        strings.ContainsAny(rotated," \n\t\r") {
        return RefreshedTaskGrant{},ErrTokenExchange
    }
    principal,err:=NewVerifier(provider).Verify(ctx,bearer)
    if err!=nil || !principal.HasAll(requiredScopes) ||
        principal.Issuer!=provider.Issuer ||
        principal.Audience!=provider.Audience ||
        principal.Subject=="" || time.Until(principal.ExpiresAt)<2*time.Second {
        return RefreshedTaskGrant{},ErrTokenExchange
    }
    owner:=sha256.Sum256([]byte("oauth:"+principal.Issuer+"\x00"+
        principal.Audience+"\x00"+principal.Subject))
    want:=hex.EncodeToString(owner[:])
    if subtle.ConstantTimeCompare([]byte(want),[]byte(ownerDigest))!=1 {
        return RefreshedTaskGrant{},ErrTokenExchange
    }
    // If present, the optional response scope cannot contradict the validated
    // access-token scopes; both must cover the original task grant.
    if responseScopes,present:=claims["scope"];present {
        raw,ok:=responseScopes.(string)
        if !ok{return RefreshedTaskGrant{},ErrTokenExchange}
        supplied:=map[string]bool{}
        for _,item:=range strings.Fields(raw){supplied[item]=true}
        for _,needed:=range requiredScopes {
            if !supplied[needed]{return RefreshedTaskGrant{},ErrTokenExchange}
        }
    }
    if !json.Valid(body) {return RefreshedTaskGrant{},ErrTokenExchange}
    return RefreshedTaskGrant{
        AccessToken:bearer,RefreshToken:rotated,Principal:principal,
        RefreshExpiresAt:time.Now().Add(time.Duration(refreshSeconds)*time.Second).UTC(),
    },nil
}
