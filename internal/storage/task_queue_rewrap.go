package storage

import (
    "context"
    "encoding/json"
    "fmt"
)

// QueueKeyState never includes raw encryption key material, nonces or task
// arguments. Only the authenticated management surface may expose it.
type QueueKeyState struct {
    ActiveKey string `json:"active_key"`
    ByKey map[string]int `json:"by_key"`
    Total int `json:"total"`
}

type QueueRewrapReport struct {
    QueueKeyState
    Rewrapped int `json:"rewrapped"`
    RemainingOldKey int `json:"remaining_old_key"`
}

// QueueKeyUsage is a non-secret projection used by the operator to determine
// whether a previous key may safely be retired from the live keyring.
// Historical WAL pages and SQLite backups require separate retention policy.
func (s *Store) QueueKeyUsage(ctx context.Context)(QueueKeyState,error) {
    state:=QueueKeyState{ByKey:map[string]int{}}
    if s.queueKeys!=nil {state.ActiveKey=s.queueKeys.active}
    rows,err:=s.db.QueryContext(ctx,
        "SELECT key_id,COUNT(*) FROM task_queue GROUP BY key_id ORDER BY key_id")
    if err!=nil{return state,err}
    defer rows.Close()
    for rows.Next() {
        var key string
        var n int
        if err=rows.Scan(&key,&n);err!=nil{return state,err}
        state.ByKey[key]=n
        state.Total+=n
    }
    if err=rows.Err();err!=nil{return state,err}
    return state,nil
}

type queuedRewrapCandidate struct {
    id string
    owner string
    revision string
    surface string
    tool string
    oldKey string
    nonce []byte
    ciphertext []byte
    taskJSON string
}

// RewrapQueuedBatch re-encrypts a bounded batch of still-queued input under
// the currently active REGATE_QUEUE_KEYS_JSON key. Both old and new keys must
// be present. Selection, AES-GCM verification/re-encryption, ciphertext
// replacement and leader validation are performed in ONE SQLite transaction.
//
// This never claims or dispatches tasks. Corrupt metadata or ciphertext
// fails the entire batch closed; no partial key migration is committed.
func (s *Store) RewrapQueuedBatch(ctx context.Context,maxItems int)(QueueRewrapReport,error) {
    report:=QueueRewrapReport{QueueKeyState:QueueKeyState{ByKey:map[string]int{}}}
    if s.queueKeys==nil {return report,ErrTaskQueueKeyUnavailable}
    if maxItems<1 || maxItems>64 {
        return report,fmt.Errorf("queue rewrap batch size must be between 1 and 64")
    }
    report.ActiveKey=s.queueKeys.active
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return report,err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return report,err}

    rows,err:=tx.QueryContext(ctx,`SELECT q.task_id,q.owner_digest,
        q.revision,q.surface,q.tool,q.key_id,q.nonce,q.ciphertext,t.record
        FROM task_queue q
        JOIN tasks t ON t.task_id=q.task_id
        WHERE q.key_id<>? ORDER BY q.seq LIMIT ?`,s.queueKeys.active,maxItems)
    if err!=nil{return report,err}
    candidates:=make([]queuedRewrapCandidate,0,maxItems)
    for rows.Next(){
        var item queuedRewrapCandidate
        if err=rows.Scan(&item.id,&item.owner,&item.revision,
            &item.surface,&item.tool,&item.oldKey,&item.nonce,
            &item.ciphertext,&item.taskJSON);err!=nil {break}
        candidates=append(candidates,item)
    }
    if err==nil{err=rows.Err()}
    if closeErr:=rows.Close();err==nil{err=closeErr}
    if err!=nil{return report,err}

    for _,item:=range candidates {
        var task TaskRecord
        if err=json.Unmarshal([]byte(item.taskJSON),&task);err!=nil {
            return report,ErrTaskQueueKeyUnavailable
        }
        if task.TaskID!=item.id || task.OwnerDigest!=item.owner ||
            task.Revision!=item.revision || task.Surface!=item.surface ||
            task.Tool!=item.tool || task.Status!="queued" ||
            task.Phase!="queued" || task.Delegation!=nil ||
            len(task.RequiredScopes)!=0 || task.RedactPII {
            return report,ErrTaskQueueKeyUnavailable
        }
        arguments,err:=s.queueKeys.open(task,item.oldKey,
            item.nonce,item.ciphertext)
        if err!=nil{return report,ErrTaskQueueKeyUnavailable}
        newKey,newNonce,sealed,err:=s.queueKeys.seal(task,arguments)
        if err!=nil{return report,err}
        result,err:=tx.ExecContext(ctx,`UPDATE task_queue SET
            key_id=?,nonce=?,ciphertext=?
            WHERE task_id=? AND key_id=?`,
            newKey,newNonce,sealed,item.id,item.oldKey)
        if err!=nil{return report,err}
        affected,err:=result.RowsAffected()
        if err!=nil || affected!=1{return report,ErrTaskTransition}
        report.Rewrapped++
    }
    usageRows,err:=tx.QueryContext(ctx,`SELECT key_id,COUNT(*)
        FROM task_queue GROUP BY key_id ORDER BY key_id`)
    if err!=nil{return report,err}
    for usageRows.Next() {
        var key string
        var count int
        if err=usageRows.Scan(&key,&count);err!=nil{break}
        report.ByKey[key]=count
        report.Total+=count
        if key!=report.ActiveKey{report.RemainingOldKey+=count}
    }
    if err==nil{err=usageRows.Err()}
    if closeErr:=usageRows.Close();err==nil{err=closeErr}
    if err!=nil{return report,err}
    if err=tx.Commit();err!=nil{return report,err}
    return report,nil
}
