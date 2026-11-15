package server

import (
    "net/http"
    "net/url"
    "sort"
    "strings"

    "github.com/burkostya/regate/internal/config"
)

// Protected resource metadata is public by specification (RFC 9728). It
// advertises authorization server identity, never client credentials or
// tokens, and derives the advertised resource from the trusted HCL revision.
func (s *Service) oauthResourceMetadata(w http.ResponseWriter,r *http.Request) {
    if r.Method!=http.MethodGet && r.Method!=http.MethodOptions {
        w.Header().Set("Allow","GET, OPTIONS")
        http.Error(w,"method not allowed",http.StatusMethodNotAllowed)
        return
    }
    cfg,_:=s.Current()
    for _,surface:=range cfg.Surfaces {
        if surface.OAuth=="" || r.URL.Path!="/.well-known/oauth-protected-resource"+surface.Path {continue}
        if r.Method==http.MethodOptions {
            w.Header().Set("Allow","GET, OPTIONS")
            w.WriteHeader(http.StatusNoContent)
            return
        }
        provider:=cfg.OAuth[surface.OAuth]
        scopes:=map[string]bool{}
        for _,scope:=range surface.RequiredScopes {scopes[scope]=true}
        for _,t:=range surface.Tools {for _,scope:=range t.RequiredScopes {scopes[scope]=true}}
        for _,res:=range surface.Resources {for _,scope:=range res.RequiredScopes {scopes[scope]=true}}
        for _,p:=range surface.Prompts {for _,scope:=range p.RequiredScopes {scopes[scope]=true}}
        published:=make([]string,0,len(scopes))
        for scope:=range scopes {published=append(published,scope)}
        sort.Strings(published)
        w.Header().Set("Access-Control-Allow-Origin","*")
        reply(w,200,map[string]any{
            "resource":provider.Audience,
            "authorization_servers":[]string{provider.Issuer},
            "scopes_supported":published,
            "bearer_methods_supported":[]string{"header"},
        })
        return
    }
    http.NotFound(w,r)
}
func resourceMetadataURL(p config.OAuth,surface config.Surface) string {
    audience,err:=url.Parse(p.Audience)
    if err!=nil || audience.Host=="" || (audience.Scheme!="https" && audience.Scheme!="http") {return ""}
    audience.Path="/.well-known/oauth-protected-resource"+surface.Path
    audience.RawPath=""
    audience.RawQuery=""
    audience.Fragment=""
    return audience.String()
}
func wwwAuthenticate(metadata,reason string) string {
    if metadata=="" {return `Bearer error="`+reason+`"`}
    // Percent-escaped resource URI is generated from validated configuration.
    return `Bearer resource_metadata="`+strings.ReplaceAll(metadata,`"`, "%22")+`", error="`+reason+`"`
}
