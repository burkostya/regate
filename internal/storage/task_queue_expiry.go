package storage

import (
    "context"
    "database/sql"
    "encoding/json"
    "fmt"
    "time"
)

const maxQueuedExpirySweep = 256
const maxQueueTTLMS = int64(24 * time.Hour / time.Millisecond)

type expiringQueueRecord struct {
    id string
    owner string
    keyID string
    task TaskRecord
}

// SweepExpiredQueued removes a bounded batch of expired, still-encrypted
// waiting tasks even if *all active execution slots remain occupied*.
// Under the SQLite leader lease it validates original task bindings and
// authenticates AES-GCM ciphertext BEFORE deleting anything. A corrupt row
// rolls the whole transaction back rather than silently discarding evidence.
// It does not claim, execute or retry any external effect.
func (s *Store) SweepExpiredQueued(ctx context.Context,limit int)(int,error) {
    if limit<1 || limit>maxQueuedExpirySweep {
        return 0,fmt.Errorf("invalid queued expiry sweep limit")
    }
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return 0,err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return 0,err}

    rows,err:=tx.QueryContext(ctx,`SELECT q.task_id,q.owner_digest,
        q.revision,q.surface,q.tool,q.key_id,t.record,t.status
        FROM task_queue q LEFT JOIN tasks t ON t.task_id=q.task_id
        ORDER BY q.seq`)
    if err!=nil{return 0,err}
    expired:=make([]expiringQueueRecord,0,limit)
    now:=time.Now()
    for rows.Next() {
        var id,owner,revision,surface,tool,keyID string
        var record,status sql.NullString
        if err=rows.Scan(&id,&owner,&revision,&surface,&tool,&keyID,
            &record,&status);err!=nil {break}
        if !record.Valid || !status.Valid {
            err=ErrTaskQueueKeyUnavailable
            break
        }
        var task TaskRecord
        if err=json.Unmarshal([]byte(record.String),&task);err!=nil{break}
        if task.TaskID!=id || task.OwnerDigest!=owner ||
            task.Revision!=revision || task.Surface!=surface ||
            task.Tool!=tool || task.Status!="queued" ||
            status.String!="queued" || task.Phase!="queued" ||
            task.TTLMS<=0 || task.TTLMS>maxQueueTTLMS ||
            task.CreatedAt.IsZero() || task.Delegation!=nil ||
            len(task.RequiredScopes)>0 || task.RedactPII {
            err=ErrTaskQueueKeyUnavailable
            break
        }
        if now.Sub(task.CreatedAt)>=time.Duration(task.TTLMS)*time.Millisecond {
            expired=append(expired,expiringQueueRecord{
                id:id,owner:owner,keyID:keyID,task:task,
            })
            if len(expired)==limit {break}
        }
    }
    if err==nil {err=rows.Err()}
    if closeErr:=rows.Close();err==nil{err=closeErr}
    if err!=nil{return 0,err}

    for _,item:=range expired {
        var nonce,ciphertext []byte
        if err=tx.QueryRowContext(ctx,`SELECT nonce,ciphertext
            FROM task_queue WHERE task_id=? AND key_id=?`,
            item.id,item.keyID).Scan(&nonce,&ciphertext);err!=nil{
            return 0,ErrTaskQueueKeyUnavailable
        }
        // An expired ciphertext is still sensitive operator evidence.
        // Never erase it merely because TTL elapsed if its authenticity
        // cannot be established.
        if _,err=s.queueKeys.open(item.task,item.keyID,nonce,ciphertext);err!=nil{
            return 0,ErrTaskQueueKeyUnavailable
        }
        item.task.Status="failed"
        item.task.Phase=""
        item.task.StatusMessage="Queued task expired before dispatch"
        item.task.Error=map[string]any{
            "code":-32000,"message":"Queued task expired",
        }
        item.task.LastUpdatedAt=now.UTC()
        raw,err:=json.Marshal(item.task)
        if err!=nil{return 0,err}
        result,err:=tx.ExecContext(ctx,`UPDATE tasks SET status='failed',record=?
            WHERE task_id=? AND status='queued'`,string(raw),item.id)
        if err!=nil{return 0,err}
        count,err:=result.RowsAffected()
        if err!=nil || count!=1{return 0,ErrTaskTransition}
        deleted,err:=tx.ExecContext(ctx,`DELETE FROM task_queue
            WHERE task_id=? AND key_id=?`,item.id,item.keyID)
        if err!=nil{return 0,err}
        removed,err:=deleted.RowsAffected()
        if err!=nil || removed!=1{return 0,ErrTaskTransition}
        if _,err=tx.ExecContext(ctx,`DELETE FROM task_queue_turns
            WHERE owner_digest=? AND NOT EXISTS(
                SELECT 1 FROM task_queue WHERE owner_digest=?)`,
            item.owner,item.owner);err!=nil{return 0,err}
    }
    if err=tx.Commit();err!=nil{return 0,err}
    return len(expired),nil
}
