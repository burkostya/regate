package engine

import (
    "errors"
    "fmt"
    "strings"

    "github.com/burkostya/regate/internal/config"
)

var ErrUntrustedDestination = errors.New("restricted data cannot reach an untrusted destination")

// For an authenticated surface, every potentially sensitive input is
// conservatively Restricted until independently verified declassification
// exists. This first version permits only explicitly Trusted(Restricted)
// MCP/HTTP effects; no string-based heuristic silently downgrades taint.
//
// Validate the *entire* effective pipeline/interceptor graph before any node
// or policy hook can dispatch an effect. A conditional dead branch is still
// checked, avoiding authorization bypass through dynamic branching.
func enforceDataTrust(cfg *config.Config,surfaceName string,tool config.Tool) error {
    surface:=cfg.Surfaces[surfaceName]
    if surface.OAuth=="" {return nil}
    require:=func(name,level string)error{
        if level!="restricted" {return fmt.Errorf("%w: %s",ErrUntrustedDestination,name)}
        return nil
    }
    checkHook:=func(h config.Hook)error{
        if h.Transport=="builtin" {return nil}
        if err:=require("hook "+h.Name,h.TrustLevel);err!=nil{return err}
        if h.Transport=="mcp" {
            u:=cfg.Upstreams[h.Upstream]
            if err:=require("upstream "+u.Name,u.TrustLevel);err!=nil{return err}
        }
        return nil
    }
    for _,i:=range cfg.Interceptors {
        if i.Surface!="" && i.Surface!=surfaceName {continue}
        if i.Tool!="" && i.Tool!=tool.Name {continue}
        if err:=checkHook(cfg.Hooks[i.Hook]);err!=nil{return err}
    }
    if tool.Pipeline=="" {
        upstream,_,_:=strings.Cut(tool.Source,".")
        u:=cfg.Upstreams[upstream]
        return require("upstream "+u.Name,u.TrustLevel)
    }
    pipeline:=cfg.Pipelines[tool.Pipeline]
    for _,n:=range pipeline.Nodes {
        switch n.Kind {
        case "policy.check":
            policy:=cfg.Policies[n.Str("policy")]
            if policy.DecisionHook!="" {
                if err:=checkHook(cfg.Hooks[policy.DecisionHook]);err!=nil{return err}
            }
        case "hook.call":
            if err:=checkHook(cfg.Hooks[n.Str("hook")]);err!=nil{return err}
        case "mcp.call":
            u:=cfg.Upstreams[n.Str("upstream")]
            if err:=require("upstream "+u.Name,u.TrustLevel);err!=nil{return err}
        case "http.call":
            if err:=require("HTTP node "+n.Name,n.Str("trust_level"));err!=nil{return err}
        }
    }
    return nil
}

// ToolNeedsDelegation means an async tool would need a reusable delegated
// credential. It is not safe to persist the caller's short-lived token in the
// task store. Until a durable capability grant exists, reject these tasks
// instead of silently attempting an unauthenticated replay.
func ToolNeedsDelegation(cfg *config.Config,surfaceName string,tool config.Tool) bool {
    isHook:=func(h config.Hook)bool {
        if h.OAuthExchange!="" {return true}
        if h.Transport=="mcp" {return cfg.Upstreams[h.Upstream].OAuthExchange!=""}
        return false
    }
    for _,i:=range cfg.Interceptors {
        if i.Surface!="" && i.Surface!=surfaceName {continue}
        if i.Tool!="" && i.Tool!=tool.Name {continue}
        if isHook(cfg.Hooks[i.Hook]) {return true}
    }
    if tool.Pipeline=="" {
        u,_,_:=strings.Cut(tool.Source,".")
        return cfg.Upstreams[u].OAuthExchange!=""
    }
    for _,n:=range cfg.Pipelines[tool.Pipeline].Nodes {
        switch n.Kind {
        case "mcp.call":
            if cfg.Upstreams[n.Str("upstream")].OAuthExchange!="" {return true}
        case "hook.call":
            if isHook(cfg.Hooks[n.Str("hook")]) {return true}
        case "policy.check":
            if isHook(cfg.Hooks[cfg.Policies[n.Str("policy")].DecisionHook]) {return true}
        }
    }
    return false
}
