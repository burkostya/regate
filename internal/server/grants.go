package server

import (
    "context"
    "fmt"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/engine"
    "github.com/burkostya/regate/internal/identity"
    "github.com/burkostya/regate/internal/storage"
)

// ProvisionTaskGrant verifies an explicitly supplied refresh credential with
// the configured authorization server BEFORE committing it to the sealed
// store. A particular task may only provision once; a missing/ambiguous IdP
// response leaves it disabled rather than silently consuming a new token.
func (s *Service) ProvisionTaskGrant(ctx context.Context,
    surface,taskID,owner,refresh string)(storage.GrantMetadata,error) {
    if err:=s.Store.RequireExecutionLeader(ctx);err!=nil{return storage.GrantMetadata{},err}
    principal,ok:=identity.FromContext(ctx)
    if !ok || !identity.HasLiveSubjectToken(ctx) {
        return storage.GrantMetadata{},identity.ErrInvalidToken
    }
    task,err:=s.Store.TaskGet(taskID)
    if err!=nil || task.Surface!=surface || task.Status!="input_required" ||
        task.Delegation==nil || !allowedOwner(task,ownerIdentity(ctx,owner)) {
        return storage.GrantMetadata{},storage.ErrGrantUnavailable
    }
    if err=identity.RequireScopes(ctx,task.RequiredScopes);err!=nil {
        return storage.GrantMetadata{},err
    }
    source,err:=s.Store.Source(task.Revision)
    if err!=nil{return storage.GrantMetadata{},storage.ErrGrantUnavailable}
    cfg,err:=config.Decode(source)
    if err!=nil{return storage.GrantMetadata{},storage.ErrGrantUnavailable}
    published,ok:=cfg.Surfaces[surface]
    if !ok || published.OAuth!=task.Delegation.Provider {
        return storage.GrantMetadata{},storage.ErrGrantUnavailable
    }
    provider,ok:=cfg.OAuth[published.OAuth]
    if !ok || provider.Issuer!=task.Delegation.Issuer ||
        provider.Audience!=task.Delegation.Audience ||
        principal.Issuer!=provider.Issuer || principal.Audience!=provider.Audience {
        return storage.GrantMetadata{},storage.ErrGrantUnavailable
    }
    eligible:=false
    for _,tool:=range published.Tools {
        if tool.Name==task.Tool {
            eligible=engine.ToolNeedsDelegation(cfg,surface,tool)
            break
        }
    }
    if !eligible {return storage.GrantMetadata{},storage.ErrGrantUnavailable}
    ownerDigest:=fingerprint(ownerIdentity(ctx,owner))
    reserved,err:=s.Store.ReserveTaskGrantProvision(ctx,taskID,ownerDigest)
    if err!=nil{return storage.GrantMetadata{},err}

    // Refresh-token grant may rotate at the IdP even if the response is lost.
    // The SQLite reservation is durable *before* the HTTP request. We do not
    // send it again after an indeterminate or failed result.
    renewed,err:=identity.RefreshTaskGrant(ctx,provider,refresh,
        ownerDigest,task.RequiredScopes)
    if err!=nil {
        _=s.Store.MarkTaskGrantIndeterminate(context.Background(),reserved)
        return storage.GrantMetadata{},identity.ErrTokenExchange
    }
    // A canceled HTTP request must not discard a verified, rotated refresh
    // token after the provider committed the rotation. Store it with a
    // separate durability context, then return metadata only.
    durable,err:=s.Store.FinishTaskGrantRefresh(context.Background(),
        reserved,renewed.RefreshToken,renewed.RefreshExpiresAt)
    if err!=nil {
        return storage.GrantMetadata{},fmt.Errorf("verified refresh credential was not durably committed: %w",err)
    }
    return durable,nil
}

// RevokeTaskGrant is owner-scoped and does not reveal the credential. It can
// run even when the task is completed/failed; lingering bearer capabilities
// must remain revocable after execution.
func (s *Service) RevokeTaskGrant(ctx context.Context,
    surface,taskID,owner string)error {
    task,err:=s.Store.TaskGet(taskID)
    if err!=nil || task.Surface!=surface || !allowedOwner(task,ownerIdentity(ctx,owner)) {
        return storage.ErrGrantUnavailable
    }
    if _,ok:=identity.FromContext(ctx);!ok {
        return identity.ErrInvalidToken
    }
    if err=identity.RequireScopes(ctx,task.RequiredScopes);err!=nil{return err}
    if err=s.Store.RequireExecutionLeader(ctx);err!=nil{return err}
    return s.Store.RevokeTaskGrant(ctx,taskID,fingerprint(ownerIdentity(ctx,owner)))
}
func (s *Service) TaskGrantState(ctx context.Context,
    surface,taskID,owner string)(storage.GrantMetadata,error) {
    task,err:=s.Store.TaskGet(taskID)
    if err!=nil || task.Surface!=surface || !allowedOwner(task,ownerIdentity(ctx,owner)) {
        return storage.GrantMetadata{},storage.ErrGrantUnavailable
    }
    if _,ok:=identity.FromContext(ctx);!ok {
        return storage.GrantMetadata{},identity.ErrInvalidToken
    }
    if err=identity.RequireScopes(ctx,task.RequiredScopes);err!=nil {
        return storage.GrantMetadata{},err
    }
    return s.Store.TaskGrantState(ctx,taskID,fingerprint(ownerIdentity(ctx,owner)))
}
