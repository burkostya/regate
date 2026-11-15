// Package identity validates per-resource OAuth access tokens. It deliberately
// does not implement an OAuth authorization server or token exchange. An
// inbound credential is never forwarded to an arbitrary MCP upstream.
package identity

import (
    "bytes"
    "context"
    "crypto"
    "crypto/ecdsa"
    "crypto/elliptic"
    "crypto/rsa"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "math/big"
    "net/http"
    "net/url"
    "os"
    "strings"
    "sync"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/netguard"
)

var ErrInvalidToken = errors.New("invalid OAuth access token")
var ErrInsufficientScope = errors.New("insufficient OAuth scope")

type principalContextKey struct{}
type Principal struct {
    Subject string
    Issuer string
    Audience string
    Scopes map[string]bool
}
func (p Principal) HasAll(scopes []string) bool {
    for _,scope:=range scopes {if !p.Scopes[scope] {return false}}
    return true
}
func WithPrincipal(ctx context.Context, p Principal) context.Context {
    return context.WithValue(ctx, principalContextKey{}, p)
}
func FromContext(ctx context.Context) (Principal,bool) {
    p,ok:=ctx.Value(principalContextKey{}).(Principal)
    return p,ok
}
func RequireScopes(ctx context.Context, scopes []string) error {
    if len(scopes)==0 {return nil}
    p,ok:=FromContext(ctx)
    if !ok || !p.HasAll(scopes) {return ErrInsufficientScope}
    return nil
}

type key struct {
    algorithm string
    public crypto.PublicKey
}
type Verifier struct {
    provider config.OAuth
    client *http.Client
    mu sync.Mutex
    keys map[string]key
    keysUntil time.Time
}
func NewVerifier(cfg config.OAuth) *Verifier {
    return &Verifier{provider:cfg,client:netguard.Client(5*time.Second)}
}
func (v *Verifier) Verify(ctx context.Context, token string) (Principal,error) {
    if token=="" || len(token)>16384 || strings.ContainsAny(token," \n\t\r") {
        return Principal{},ErrInvalidToken
    }
    if strings.Count(token,".")==2 && v.provider.JWKSURL!="" {
        return v.verifyJWT(ctx,token)
    }
    if v.provider.IntrospectionURL!="" {
        return v.introspect(ctx,token)
    }
    return Principal{},ErrInvalidToken
}

func decodeJSONObject(blob []byte) (map[string]any,error) {
    d:=json.NewDecoder(bytes.NewReader(blob))
    d.UseNumber()
    var obj map[string]any
    if err:=d.Decode(&obj);err!=nil {return nil,err}
    if obj==nil {return nil,errors.New("JSON object required")}
    var excess any
    if err:=d.Decode(&excess);err!=io.EOF {return nil,errors.New("unexpected trailing JSON")}
    return obj,nil
}
func claimTimestamp(v any) (int64,bool) {
    n,ok:=v.(json.Number)
    if !ok {return 0,false}
    value,err:=n.Int64()
    return value,err==nil
}
func permittedAudience(raw any,wanted string) bool {
    switch v:=raw.(type) {
    case string: return v==wanted
    case []any:
        for _,item:=range v {if value,ok:=item.(string);ok && value==wanted {return true}}
    }
    return false
}
func scopesFromClaims(c map[string]any) map[string]bool {
    result:=map[string]bool{}
    if raw,ok:=c["scope"].(string);ok {
        for _,scope:=range strings.Fields(raw){result[scope]=true}
    }
    if raw,ok:=c["scp"].([]any);ok {
        for _,item:=range raw {if scope,ok:=item.(string);ok && scope!="" {result[scope]=true}}
    }
    return result
}
func (v *Verifier) principalFromClaims(c map[string]any) (Principal,error) {
    now:=time.Now().Unix()
    issuer,_:=c["iss"].(string)
    subject,_:=c["sub"].(string)
    exp,ok:=claimTimestamp(c["exp"])
    if !ok || exp<=now || issuer!=v.provider.Issuer || subject=="" ||
        !permittedAudience(c["aud"],v.provider.Audience) {
        return Principal{},ErrInvalidToken
    }
    if nbf,present:=c["nbf"];present {
        n,ok:=claimTimestamp(nbf)
        if !ok || n>now+30 {return Principal{},ErrInvalidToken}
    }
    if iat,present:=c["iat"];present {
        n,ok:=claimTimestamp(iat)
        if !ok || n>now+30 {return Principal{},ErrInvalidToken}
    }
    return Principal{Subject:subject,Issuer:issuer,Audience:v.provider.Audience,Scopes:scopesFromClaims(c)},nil
}

func (v *Verifier) verifyJWT(ctx context.Context,token string)(Principal,error) {
    parts:=strings.Split(token,".")
    if len(parts)!=3 {return Principal{},ErrInvalidToken}
    headerBlob,err:=base64.RawURLEncoding.DecodeString(parts[0])
    if err!=nil || len(headerBlob)>4096{return Principal{},ErrInvalidToken}
    header,err:=decodeJSONObject(headerBlob)
    if err!=nil {return Principal{},ErrInvalidToken}
    algorithm,_:=header["alg"].(string)
    kid,_:=header["kid"].(string)
    if kid=="" || len(kid)>256 || (algorithm!="RS256" && algorithm!="PS256" && algorithm!="ES256") ||
        header["crit"]!=nil {return Principal{},ErrInvalidToken}
    body,err:=base64.RawURLEncoding.DecodeString(parts[1])
    if err!=nil || len(body)>65536 {return Principal{},ErrInvalidToken}
    signature,err:=base64.RawURLEncoding.DecodeString(parts[2])
    if err!=nil || len(signature)>1024 {return Principal{},ErrInvalidToken}
    value,err:=v.keyFor(ctx,kid)
    if err!=nil || value.algorithm!=algorithm{return Principal{},ErrInvalidToken}
    hash:=sha256.Sum256([]byte(parts[0]+"."+parts[1]))
    verified:=false
    switch pub:=value.public.(type) {
    case *rsa.PublicKey:
        if algorithm=="RS256" {verified=rsa.VerifyPKCS1v15(pub,crypto.SHA256,hash[:],signature)==nil}
        if algorithm=="PS256" {verified=rsa.VerifyPSS(pub,crypto.SHA256,hash[:],signature,nil)==nil}
    case *ecdsa.PublicKey:
        if algorithm=="ES256" && len(signature)==64 {
            rr:=new(big.Int).SetBytes(signature[:32])
            ss:=new(big.Int).SetBytes(signature[32:])
            verified=ecdsa.Verify(pub,hash[:],rr,ss)
        }
    }
    if !verified{return Principal{},ErrInvalidToken}
    claims,err:=decodeJSONObject(body)
    if err!=nil{return Principal{},ErrInvalidToken}
    return v.principalFromClaims(claims)
}
type jwkDocument struct {
    Keys []struct{
        Kid string `json:"kid"`
        Kty string `json:"kty"`
        Alg string `json:"alg"`
        Use string `json:"use"`
        KeyOps []string `json:"key_ops"`
        N string `json:"n"`
        E string `json:"e"`
        Crv string `json:"crv"`
        X string `json:"x"`
        Y string `json:"y"`
    } `json:"keys"`
}
func (v *Verifier) keyFor(ctx context.Context,kid string)(key,error) {
    v.mu.Lock()
    defer v.mu.Unlock()
    if v.keys!=nil && time.Now().Before(v.keysUntil) {
        if k,ok:=v.keys[kid];ok{return k,nil}
    }
    if err:=v.fetchKeys(ctx);err!=nil{return key{},err}
    k,ok:=v.keys[kid]
    if !ok{return key{},ErrInvalidToken}
    return k,nil
}
func (v *Verifier) fetchKeys(ctx context.Context) error {
    if err:=netguard.ValidateURL(v.provider.JWKSURL);err!=nil {return err}
    req,err:=http.NewRequestWithContext(ctx,http.MethodGet,v.provider.JWKSURL,nil)
    if err!=nil{return err}
    resp,err:=v.client.Do(req)
    if err!=nil{return err}
    defer resp.Body.Close()
    if resp.StatusCode!=http.StatusOK {return ErrInvalidToken}
    data,err:=io.ReadAll(io.LimitReader(resp.Body,(1<<20)+1))
    if err!=nil || len(data)>1<<20{return ErrInvalidToken}
    var doc jwkDocument
    if err:=json.Unmarshal(data,&doc);err!=nil{return ErrInvalidToken}
    if len(doc.Keys)==0 || len(doc.Keys)>64 {return ErrInvalidToken}
    verified:=make(map[string]key,len(doc.Keys))
    for _,item:=range doc.Keys {
        if item.Kid=="" || len(item.Kid)>256 {continue}
        if _,dup:=verified[item.Kid];dup {return ErrInvalidToken}
        if item.Use!="" && item.Use!="sig" {continue}
        if len(item.KeyOps)>0 {
            canVerify:=false
            for _,op:=range item.KeyOps {if op=="verify" {canVerify=true}}
            if !canVerify {continue}
        }
        switch item.Kty {
        case "RSA":
            if item.Alg!="" && item.Alg!="RS256" && item.Alg!="PS256" {continue}
            n,ne:=base64.RawURLEncoding.DecodeString(item.N)
            e,ee:=base64.RawURLEncoding.DecodeString(item.E)
            if ne!=nil || ee!=nil || len(e)==0 || len(e)>4 {continue}
            exponent:=int(new(big.Int).SetBytes(e).Int64())
            modulus:=new(big.Int).SetBytes(n)
            if modulus.BitLen()<2048 || modulus.BitLen()>8192 || exponent<3 || exponent%2==0 {continue}
            alg:=item.Alg
            if alg=="" {alg="RS256"}
            verified[item.Kid]=key{algorithm:alg,public:&rsa.PublicKey{N:modulus,E:exponent}}
        case "EC":
            if item.Crv!="P-256" || (item.Alg!="" && item.Alg!="ES256") {continue}
            xb,xe:=base64.RawURLEncoding.DecodeString(item.X)
            yb,ye:=base64.RawURLEncoding.DecodeString(item.Y)
            if xe!=nil || ye!=nil || len(xb)!=32 || len(yb)!=32 {continue}
            x:=new(big.Int).SetBytes(xb)
            y:=new(big.Int).SetBytes(yb)
            curve:=elliptic.P256()
            if !curve.IsOnCurve(x,y) {continue}
            verified[item.Kid]=key{algorithm:"ES256",public:&ecdsa.PublicKey{Curve:curve,X:x,Y:y}}
        }
    }
    if len(verified)==0{return ErrInvalidToken}
    v.keys=verified
    v.keysUntil=time.Now().Add(3*time.Minute)
    return nil
}
func (v *Verifier) introspect(ctx context.Context,token string)(Principal,error) {
    if err:=netguard.ValidateURL(v.provider.IntrospectionURL);err!=nil {return Principal{},ErrInvalidToken}
    id:=os.Getenv(v.provider.ClientIDEnv)
    secret:=os.Getenv(v.provider.ClientSecretEnv)
    if id=="" || secret=="" {return Principal{},ErrInvalidToken}
    params:=url.Values{"token":{token},"token_type_hint":{"access_token"}}
    req,err:=http.NewRequestWithContext(ctx,http.MethodPost,v.provider.IntrospectionURL,strings.NewReader(params.Encode()))
    if err!=nil{return Principal{},ErrInvalidToken}
    req.Header.Set("Content-Type","application/x-www-form-urlencoded")
    req.SetBasicAuth(id,secret)
    resp,err:=v.client.Do(req)
    if err!=nil{return Principal{},ErrInvalidToken}
    defer resp.Body.Close()
    if resp.StatusCode!=http.StatusOK{return Principal{},ErrInvalidToken}
    data,err:=io.ReadAll(io.LimitReader(resp.Body,65537))
    if err!=nil || len(data)>65536{return Principal{},ErrInvalidToken}
    claims,err:=decodeJSONObject(data)
    if err!=nil || claims["active"]!=true {return Principal{},ErrInvalidToken}
    return v.principalFromClaims(claims)
}
// Check verifies that an otherwise successfully authenticated principal holds
// every required scope; the denial never reveals the missing token's identity.
func Check(p Principal,needed []string) error {
    if !p.HasAll(needed) {return fmt.Errorf("%w",ErrInsufficientScope)}
    return nil
}
