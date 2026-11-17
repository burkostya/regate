package engine

import (
    "errors"
    "fmt"
    "sort"
    "strings"

    "github.com/burkostya/regate/internal/config"
)

var ErrUntrustedDestination = errors.New("restricted data cannot reach an untrusted destination")

// walkExecutionPipeline checks the transitive executable call graph, not just
// the published top-level pipeline. Using a visited set prevents redundant
// checks for shared callees; cycles and missing targets fail closed even if
// an invalid Config somehow reaches the runtime without compilation.
func walkExecutionPipeline(cfg *config.Config, root string, inspect func(config.Node) error) error {
    done := map[string]bool{}
    active := map[string]bool{}
    var walk func(string) error
    walk = func(name string) error {
        if active[name] { return fmt.Errorf("cyclic pipeline call at %s", name) }
        if done[name] { return nil }
        p, exists := cfg.Pipelines[name]
        if !exists { return fmt.Errorf("missing called pipeline %s", name) }
        active[name] = true
        defer delete(active, name)
        names := make([]string,0,len(p.Nodes))
        for nodeName := range p.Nodes {names=append(names,nodeName)}
        sort.Strings(names)
        for _, nodeName := range names {
            node := p.Nodes[nodeName]
            if err := inspect(node); err != nil { return err }
            if node.Kind == "pipeline.call" || (node.Kind == "map" && node.Str("pipeline") != "") {
                if err := walk(node.Str("pipeline")); err != nil { return err }
            }
        }
        done[name] = true
        return nil
    }
    return walk(root)
}

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
    return walkExecutionPipeline(cfg,tool.Pipeline,func(n config.Node)error {
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
        return nil
    })
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
    requiresDelegation:=false
    err:=walkExecutionPipeline(cfg,tool.Pipeline,func(n config.Node)error {
        switch n.Kind {
        case "mcp.call":
            if cfg.Upstreams[n.Str("upstream")].OAuthExchange!="" {requiresDelegation=true}
        case "hook.call":
            if isHook(cfg.Hooks[n.Str("hook")]) {requiresDelegation=true}
        case "policy.check":
            if isHook(cfg.Hooks[cfg.Policies[n.Str("policy")].DecisionHook]) {requiresDelegation=true}
        }
        return nil
    })
    // Invalid or mutable executable graph is not safe to queue without
    // delegated execution credentials and an immutable published revision.
    return requiresDelegation || err!=nil
}
