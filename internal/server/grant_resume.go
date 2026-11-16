package server

import (
    "context"
    "fmt"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/engine"
    "github.com/burkostya/regate/internal/identity"
    "github.com/burkostya/regate/internal/storage"
)

// ResumeTaskWithGrant is an explicit, authenticated MANAGEMENT operation
// that provides new user-input for one previously suspended delegated task.
// It never retries a crashed working task or an unknown external effect.
// Refreshed OAuth authority is bound to the original task and revision.
func (s *Service) ResumeTaskWithGrant(ctx context.Context,
    taskID string,value any)(map[string]any,error) {
    if err:=s.Store.RequireExecutionLeader(ctx);err!=nil{return nil,err}

    // Invalid continuation input must not consume a rotating refresh token.
    response:=map[string]any{"resume":map[string]any{
        "action":"accept","content":value,
    }}
    s.tasksMu.Lock()
    task,err:=s.Store.TaskGet(taskID)
    if err!=nil || task.Status!="input_required" || task.RunID=="" ||
        task.Delegation==nil {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    source,err:=s.Store.Source(task.Revision)
    if err!=nil {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    cfg,err:=config.Decode(source)
    if err!=nil {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    published,ok:=cfg.Surfaces[task.Surface]
    if !ok || published.OAuth!=task.Delegation.Provider {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    provider,ok:=cfg.OAuth[published.OAuth]
    if !ok || provider.Issuer!=task.Delegation.Issuer ||
        provider.Audience!=task.Delegation.Audience {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    needsGrant:=false
    for _,tool:=range published.Tools {
        if tool.Name==task.Tool {
            needsGrant=engine.ToolNeedsDelegation(cfg,task.Surface,tool)
            break
        }
    }
    if !needsGrant {
        s.tasksMu.Unlock()
        return nil,storage.ErrGrantUnavailable
    }
    reserved,refreshToken,err:=s.Store.BeginTaskGrantContinuation(
        ctx,taskID,task.OwnerDigest)
    s.tasksMu.Unlock()
    if err!=nil {return nil,err}

    refreshed,err:=identity.RefreshTaskGrant(
        ctx,provider,refreshToken,task.OwnerDigest,task.RequiredScopes)
    if err!=nil {
        _=s.Store.MarkTaskGrantIndeterminate(context.Background(),reserved)
        s.failGrantContinuation(taskID)
        return nil,identity.ErrTokenExchange
    }
    next,err:=s.Store.FinishTaskGrantRefresh(
        context.Background(),reserved,refreshed.RefreshToken,
        refreshed.RefreshExpiresAt)
    if err!=nil {
        s.failGrantContinuation(taskID)
        return nil,fmt.Errorf("verified delegated credential not durable: %w",err)
    }

    // No bearer is persisted or returned. The new Regate-audience credential
    // can now perform RFC8693 exchange to the original upstream audience in
    // the same in-process engine invocation. Its deadline is token-bound.
    run:=identity.WithPrincipal(context.Background(),refreshed.Principal)
    run=identity.WithSubjectToken(run,refreshed.AccessToken)
    run=engine.WithGrantExecution(run,taskID,task.OwnerDigest,next.Generation)
    if err=s.Engine.CheckGrantExecution(run);err!=nil {
        s.failGrantContinuation(taskID)
        return nil,err
    }
    // The owner identity is independently proven by RefreshTaskGrant from the
    // IdP's signed/introspected access token. No admin bearer is forwarded.
    owner:=ownerIdentity(run,"")
    if !allowedOwner(task,owner) {
        s.failGrantContinuation(taskID)
        return nil,identity.ErrTokenExchange
    }
    result,err:=s.TaskUpdateWithContext(run,taskID,owner,response)
    if err!=nil {
        s.failGrantContinuation(taskID)
        return nil,err
    }
    _=result
    return map[string]any{"taskId":taskID,"status":"working"},nil
}

// An indeterminate remote token rotation is never retried, even if a process
// crashes after reserving the task. Report a generic failure; preserve the
// original checkpoint and journal for independent operator inspection.
func (s *Service) failGrantContinuation(taskID string) {
    s.tasksMu.Lock()
    defer s.tasksMu.Unlock()
    task,err:=s.Store.TaskGet(taskID)
    if err!=nil || task.Status!="working" ||
        task.Phase!="refreshing_grant" {return}
    task.Phase=""
    task.Status="failed"
    task.StatusMessage="Delegated authorization requires fresh consent"
    task.Error=map[string]any{
        "code":-32000,
        "message":"Delegated credential unavailable; create a new authorized task",
    }
    _=s.Store.TaskPut(task)
}
