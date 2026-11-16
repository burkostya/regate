package engine

import (
    "context"
    "fmt"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/expr"
    "github.com/burkostya/regate/internal/storage"
)

// CompensateMCP executes one explicitly authorized, HCL-declared inverse
// action against the SAME endpoint as the applied source effect. It is a
// separate effect, not a rollback of the original event or a generic undo.
// Both original identity and durable intent must match before dispatch.
func (r *Runtime) CompensateMCP(ctx context.Context,cfg *config.Config,
    originalID,contract string,originalIntent map[string]any,
    actor,reason string)(storage.Effect,error) {
    if r.Store==nil {return storage.Effect{},fmt.Errorf("durable store required")}
    if err:=r.Store.RequireExecutionLeader(ctx);err!=nil{return storage.Effect{},err}
    rule,ok:=cfg.Compensations[contract]
    if !ok || !rule.Enabled {return storage.Effect{},storage.ErrInvalidEvidence}
    upstream,ok:=cfg.Upstreams[rule.Upstream]
    if !ok {return storage.Effect{},storage.ErrInvalidEvidence}
    source,err:=r.Store.EffectGet(originalID)
    if err!=nil{return storage.Effect{},err}
    target:=storage.TargetDigest("mcp",upstream.URL)
    if source.Status!="resolved_applied" || source.TargetDigest!=target ||
        source.Operation!=rule.ForOperation || source.Compensates!="" {
        return storage.Effect{},storage.ErrInvalidEvidence
    }
    digest,err:=storage.DigestIntent(originalIntent)
    if err!=nil || source.IntentDigest!=digest {return storage.Effect{},storage.ErrInvalidEvidence}
    identity,ok:=jsonPointer(originalIntent,rule.IntentPointer)
    if !ok || !readbackIdentity(identity) {
        return storage.Effect{},storage.ErrInvalidEvidence
    }
    if err=checkOutbound(upstream.URL);err!=nil{return storage.Effect{},err}
    conn,err:=r.connFor(ctx,cfg,upstream)
    if err!=nil{return storage.Effect{},err}
    args:=map[string]any{rule.Argument:identity}
    attempt,err:=r.Store.BeginCompensation(originalID,rule.ForOperation,
        target,upstream.Name+"."+rule.Tool,originalIntent,args,actor,reason)
    if err!=nil{return storage.Effect{},err}
    complete:=func(state string,cause error)(storage.Effect,error) {
        explanation:=""
        if cause!=nil {explanation=expr.RedactDiagnostic(cause.Error())}
        if e:=r.Store.CompleteEffect(attempt.ID,state,explanation);e!=nil {
            return storage.Effect{},fmt.Errorf("compensation outcome not durable: %w",e)
        }
        saved,e:=r.Store.EffectGet(attempt.ID)
        if e!=nil {return storage.Effect{},e}
        return saved,cause
    }
    if err=r.Store.RequireExecutionLeader(ctx);err!=nil {
        return complete("unknown",err)
    }
    raw,e:=conn.CallTool(ctx,rule.Tool,args)
    if e==nil && (raw==nil || raw["isError"]==true) {
        e=fmt.Errorf("compensation remote result not confirmed")
    }
    if e!=nil {return complete("unknown",e)}
    return complete("confirmed",nil)
}
