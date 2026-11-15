package identity

import (
    "context"
    "encoding/json"
    "errors"
    "io"
    "net/http"
    "net/url"
    "os"
    "strings"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/netguard"
)

// The subject token is available only to trusted in-process adapters after
// Verify has succeeded. It is intentionally not included in Principal, trace,
// task snapshots, responses or persisted configuration.
type subjectTokenContextKey struct{}
func WithSubjectToken(ctx context.Context,verifiedToken string) context.Context {
    if _,ok:=FromContext(ctx);!ok {return ctx}
    return context.WithValue(ctx,subjectTokenContextKey{},verifiedToken)
}
func subjectToken(ctx context.Context) string {
    token,_:=ctx.Value(subjectTokenContextKey{}).(string)
    return token
}

var ErrTokenExchange = errors.New("delegated OAuth authorization unavailable")
const tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
const accessTokenType = "urn:ietf:params:oauth:token-type:access_token"

// Exchange performs RFC 8693 OAuth token exchange for one trusted upstream.
// It rejects non-audience-bound and non-issuer-verified responses, rather than
// forwarding a Regate bearer token to an unrelated upstream.
func Exchange(ctx context.Context,provider config.OAuth,targetAudience string,neededScopes []string) (string,error) {
    principal,ok:=FromContext(ctx)
    original:=subjectToken(ctx)
    if !ok || original=="" || principal.Issuer!=provider.Issuer ||
        targetAudience=="" || len(neededScopes)==0 || provider.TokenEndpoint=="" ||
        principal.Subject=="" {
        return "",ErrTokenExchange
    }
    clientID:=os.Getenv(provider.ExchangeClientIDEnv)
    clientSecret:=os.Getenv(provider.ExchangeClientSecretEnv)
    if clientID=="" || clientSecret=="" {return "",ErrTokenExchange}
    if err:=netguard.ValidateURL(provider.TokenEndpoint);err!=nil{return "",ErrTokenExchange}

    values:=url.Values{
        "grant_type":{tokenExchangeGrant},
        "subject_token":{original},
        "subject_token_type":{accessTokenType},
        "requested_token_type":{accessTokenType},
        "audience":{targetAudience},
        "scope":{strings.Join(neededScopes," ")},
    }
    request,err:=http.NewRequestWithContext(ctx,http.MethodPost,provider.TokenEndpoint,strings.NewReader(values.Encode()))
    if err!=nil{return "",ErrTokenExchange}
    request.Header.Set("Content-Type","application/x-www-form-urlencoded")
    request.SetBasicAuth(clientID,clientSecret)
    httpClient:=netguard.Client(6*time.Second)
    response,err:=httpClient.Do(request)
    if err!=nil{return "",ErrTokenExchange}
    defer response.Body.Close()
    if response.StatusCode!=http.StatusOK {return "",ErrTokenExchange}
    body,err:=io.ReadAll(io.LimitReader(response.Body,65537))
    if err!=nil || len(body)>65536 {return "",ErrTokenExchange}
    var result struct{
        AccessToken string `json:"access_token"`
        IssuedTokenType string `json:"issued_token_type"`
        TokenType string `json:"token_type"`
        ExpiresIn int64 `json:"expires_in"`
    }
    if err:=json.Unmarshal(body,&result);err!=nil{return "",ErrTokenExchange}
    if result.IssuedTokenType!=accessTokenType ||
        result.TokenType!="Bearer" || result.AccessToken=="" ||
        result.AccessToken==original || result.ExpiresIn<=0 || result.ExpiresIn>86400 {
        return "",ErrTokenExchange
    }
    // Verify the exchanged credential *against the upstream resource audience*.
    // A token intended for the incoming Regate audience is never accepted.
    target:=provider
    target.Audience=targetAudience
    authorized,err:=NewVerifier(target).Verify(ctx,result.AccessToken)
    if err!=nil || authorized.Subject!=principal.Subject || !authorized.HasAll(neededScopes) {
        return "",ErrTokenExchange
    }
    return result.AccessToken,nil
}
