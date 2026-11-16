package storage

import (
    "context"
    "encoding/json"
    "time"
)

// BeginTaskGrantContinuation atomically reserves BOTH the task checkpoint and
// a potentially rotating OAuth refresh credential. A second task/update or
// grant-resume cannot execute the same input_required checkpoint while an IdP
// request is in flight. Neither SQLite observer nor takeover may replay the
// reserved task; interrupted 'working' tasks fail closed on recovery.
func (s *Store) BeginTaskGrantContinuation(ctx context.Context,
    taskID,owner string)(GrantMetadata,string,error) {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},"",err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},"",err}
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil || task.Status!="input_required" || task.Phase!="" ||
        len(task.RunID)!=24 {return GrantMetadata{},"",ErrGrantUnavailable}

    // Refuse consumption of a refresh token when the original checkpoint was
    // already removed (e.g. by another completed continuation).
    var checkpoint string
    if err=tx.QueryRow("SELECT record FROM suspended WHERE run_id=?",task.RunID).
        Scan(&checkpoint);err!=nil || checkpoint==""{
        return GrantMetadata{},"",ErrGrantUnavailable
    }

    grant,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return GrantMetadata{},"",ErrGrantUnavailable}
    if err=validateGrantOwner(grant,task,owner);err!=nil{return GrantMetadata{},"",err}
    if grant.State!="active"{return GrantMetadata{},"",ErrGrantRefreshPending}
    secret,err:=s.grantKeys.open(grant.GrantMetadata,grant.nonce,grant.ciphertext)
    if err!=nil{return GrantMetadata{},"",err}

    result,err:=tx.Exec(`UPDATE task_grants SET state='refreshing',updated_ns=?
        WHERE task_id=? AND owner_digest=? AND generation=? AND state='active'`,
        time.Now().UnixNano(),taskID,owner,grant.Generation)
    if err!=nil{return GrantMetadata{},"",err}
    n,err:=result.RowsAffected()
    if err!=nil || n!=1{return GrantMetadata{},"",ErrGrantConflict}

    task.Status="working"
    task.Phase="refreshing_grant"
    task.StatusMessage="Authorizing delegated continuation"
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return GrantMetadata{},"",err}
    result,err=tx.Exec(`UPDATE tasks SET status='working',record=?
        WHERE task_id=? AND status='input_required'`,string(raw),taskID)
    if err!=nil{return GrantMetadata{},"",err}
    n,err=result.RowsAffected()
    if err!=nil || n!=1{return GrantMetadata{},"",ErrGrantConflict}
    if err=tx.Commit();err!=nil{return GrantMetadata{},"",err}
    grant.State="refreshing"
    return grant.GrantMetadata,secret,nil
}

// RequireActiveTaskGrant fences all later remote-effect boundaries to the
// exact verified, still-active generation. A grant revoked or rotated after
// authorization cannot silently permit the next effect. Already in-flight
// HTTP requests still need remote provider fencing or idempotency keys.
func (s *Store) RequireActiveTaskGrant(ctx context.Context,
    taskID,owner string,generation int64)error {
    if err:=s.RequireExecutionLeader(ctx);err!=nil{return err}
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil{return ErrGrantUnavailable}
    grant,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return ErrGrantUnavailable}
    if grant.State!="active" || grant.Generation!=generation {
        return ErrGrantUnavailable
    }
    return validateGrantOwner(grant,task,owner)
}
