package storage

import (
    "context"
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "os"
    "time"
)

var ErrTaskQueueFull = errors.New("asynchronous task waiting queue is full")
var ErrTaskQueueExpired = errors.New("queued task expired before dispatch")
var ErrTaskQueueKeyUnavailable = errors.New("task queue encryption key unavailable")
const taskQueueKeyEnv = "REGATE_QUEUE_KEYS_JSON"
const maxQueueArgumentBytes = 1 << 20
const maxQueuedTasksLimit = 4096

// TaskQueueKeyRing has a distinct domain and environment variable from the
// OAuth grant vault. Queue secrets must not be conflated with refresh-token
// grant keys. Keys are never persisted to the database or emitted over MCP.
type TaskQueueKeyRing struct {
    active string
    keys map[string][]byte
}
func LoadTaskQueueKeyRing()(*TaskQueueKeyRing,error) {
    raw:=os.Getenv(taskQueueKeyEnv)
    if raw=="" {return nil,nil}
    if len(raw)>4096 {return nil,ErrTaskQueueKeyUnavailable}
    var cfg struct {
        Active string `json:"active"`
        Keys map[string]string `json:"keys"`
    }
    if err:=json.Unmarshal([]byte(raw),&cfg);err!=nil{return nil,ErrTaskQueueKeyUnavailable}
    if !validGrantKeyID(cfg.Active) || len(cfg.Keys)==0 || len(cfg.Keys)>8 {
        return nil,ErrTaskQueueKeyUnavailable
    }
    ring:=&TaskQueueKeyRing{active:cfg.Active,keys:map[string][]byte{}}
    for id,val:=range cfg.Keys {
        if !validGrantKeyID(id){return nil,ErrTaskQueueKeyUnavailable}
        key,err:=base64.StdEncoding.Strict().DecodeString(val)
        if err!=nil || len(key)!=32{return nil,ErrTaskQueueKeyUnavailable}
        ring.keys[id]=key
    }
    if _,ok:=ring.keys[ring.active];!ok{return nil,ErrTaskQueueKeyUnavailable}
    return ring,nil
}
func (s *Store) HasTaskQueueKeys()bool {return s.queueKeys!=nil}

func queuedAAD(task TaskRecord,keyID string)[]byte {
    data,_:=json.Marshal(struct {
        Domain string `json:"domain"`
        TaskID string `json:"task_id"`
        Owner string `json:"owner"`
        Revision string `json:"revision"`
        Surface string `json:"surface"`
        Tool string `json:"tool"`
        CreatedNS int64 `json:"created_ns"`
        TTLMS int64 `json:"ttl_ms"`
        ScopesDigest string `json:"scopes_digest"`
        RedactPII bool `json:"redact_pii"`
        Key string `json:"key"`
    }{
        Domain:"regate/task-queue/aes-256-gcm/v1",
        TaskID:task.TaskID,Owner:task.OwnerDigest,Revision:task.Revision,
        Surface:task.Surface,Tool:task.Tool,
        CreatedNS:task.CreatedAt.UnixNano(),TTLMS:task.TTLMS,
        ScopesDigest:digestTaskScopes(task.RequiredScopes),
        RedactPII:task.RedactPII,Key:keyID,
    })
    return data
}
func (k *TaskQueueKeyRing) seal(task TaskRecord,args map[string]any)(string,[]byte,[]byte,error) {
    if k==nil {return "",nil,nil,ErrTaskQueueKeyUnavailable}
    raw,err:=json.Marshal(args)
    if err!=nil || len(raw)>maxQueueArgumentBytes {
        return "",nil,nil,fmt.Errorf("queued task input exceeds supported JSON bounds")
    }
    keyID:=k.active
    cipherBlock,err:=aes.NewCipher(k.keys[keyID])
    if err!=nil{return "",nil,nil,ErrTaskQueueKeyUnavailable}
    gcm,err:=cipher.NewGCM(cipherBlock)
    if err!=nil{return "",nil,nil,ErrTaskQueueKeyUnavailable}
    nonce:=make([]byte,gcm.NonceSize())
    if _,err=io.ReadFull(rand.Reader,nonce);err!=nil{return "",nil,nil,err}
    return keyID,nonce,gcm.Seal(nil,nonce,raw,queuedAAD(task,keyID)),nil
}
func (k *TaskQueueKeyRing) open(task TaskRecord,keyID string,nonce,ciphertext []byte)(map[string]any,error) {
    if k==nil{return nil,ErrTaskQueueKeyUnavailable}
    key,ok:=k.keys[keyID]
    if !ok {return nil,ErrTaskQueueKeyUnavailable}
    block,err:=aes.NewCipher(key)
    if err!=nil{return nil,ErrTaskQueueKeyUnavailable}
    gcm,err:=cipher.NewGCM(block)
    if err!=nil{return nil,ErrTaskQueueKeyUnavailable}
    if len(nonce)!=gcm.NonceSize() || len(ciphertext)>maxQueueArgumentBytes+gcm.Overhead() {
        return nil,ErrTaskQueueKeyUnavailable
    }
    raw,err:=gcm.Open(nil,nonce,ciphertext,queuedAAD(task,keyID))
    if err!=nil{return nil,ErrTaskQueueKeyUnavailable}
    var args map[string]any
    err=json.Unmarshal(raw,&args)
    for i:=range raw {raw[i]=0}
    if err!=nil || args==nil{return nil,ErrTaskQueueKeyUnavailable}
    return args,nil
}

// QueueDepth is a non-secret projection of outstanding accepted work.
func (s *Store) QueueDepth(ctx context.Context)(int,error) {
    var count int
    err:=s.db.QueryRowContext(ctx,"SELECT COUNT(*) FROM task_queue").Scan(&count)
    return count,err
}

// AdmitOrQueueTask returns true only when a task is admitted to the bounded
// encrypted FIFO queue; false means immediately running. Rejected work gets
// no task handle. Credentials or raw input must never be stored as plaintext.
//
// No OAuth-protected task is admitted to the deferred queue: a user-scoped
// bearer cannot be safely reused after request expiry without a separate
// resource-scoped grant/reauthorization workflow.
func (s *Store) AdmitOrQueueTask(ctx context.Context,task TaskRecord,
    args map[string]any,maxActive,maxQueued int)(bool,error) {
    if err:=validTaskLimit(maxActive);err!=nil{return false,err}
    if maxQueued<0 || maxQueued>maxQueuedTasksLimit {
        return false,fmt.Errorf("invalid waiting queue limit")
    }
    if len(task.TaskID)!=24 || task.OwnerDigest=="" || task.Revision=="" ||
        task.Status!="working" || task.Phase!="" {
        return false,fmt.Errorf("invalid queued task")
    }
    if task.CreatedAt.IsZero(){task.CreatedAt=time.Now().UTC()}
    task.LastUpdatedAt=time.Now().UTC()
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return false,err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return false,err}
    count,err:=workingTaskCountTx(tx)
    if err!=nil{return false,err}
    // FIFO precedence must hold even when a slot becomes free between
    // queue admission and a dispatcher claim. New work may not cut ahead of
    // earlier, already accepted waiting work.
    var waiting int
    if err=tx.QueryRow("SELECT COUNT(*) FROM task_queue").Scan(&waiting);err!=nil{return false,err}
    queued:=false
    var keyID string
    var nonce,ciphertext []byte
    if count>=maxActive || waiting>0 {
        if maxQueued==0 {return false,ErrTaskCapacity}
        if s.queueKeys==nil {return false,ErrTaskQueueKeyUnavailable}
        if task.Delegation!=nil || len(task.RequiredScopes)!=0 || task.RedactPII {
            return false,ErrTaskCapacity
        }
        if waiting>=maxQueued{return false,ErrTaskQueueFull}
        queued=true
        task.Status="queued"
        task.Phase="queued"
        task.StatusMessage="Waiting for available execution capacity"
        keyID,nonce,ciphertext,err=s.queueKeys.seal(task,args)
        if err!=nil{return false,err}
    }
    raw,err:=json.Marshal(task)
    if err!=nil{return false,err}
    if _,err=tx.Exec("INSERT INTO tasks(task_id,status,record) VALUES(?,?,?)",
        task.TaskID,task.Status,string(raw));err!=nil{return false,err}
    if queued {
        _,err=tx.Exec(`INSERT INTO task_queue(task_id,owner_digest,revision,surface,tool,
            key_id,nonce,ciphertext,created_ns) VALUES(?,?,?,?,?,?,?,?,?)`,
            task.TaskID,task.OwnerDigest,task.Revision,task.Surface,task.Tool,
            keyID,nonce,ciphertext,time.Now().UnixNano())
        if err!=nil{return false,err}
    }
    if err=tx.Commit();err!=nil{return false,err}
    return queued,nil
}

// ClaimNextQueued atomically frees a FIFO encrypted record only after the
// corresponding task switches to working. A crash after the transaction and
// before execution marks the task failed, instead of re-running an ambiguous
// effect; unclaimed queued jobs survive restarts and leader takeover.
func (s *Store) ClaimNextQueued(ctx context.Context,maxActive int)(TaskRecord,map[string]any,error) {
    if err:=validTaskLimit(maxActive);err!=nil{return TaskRecord{},nil,err}
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return TaskRecord{},nil,err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return TaskRecord{},nil,err}
    active,err:=workingTaskCountTx(tx)
    if err!=nil{return TaskRecord{},nil,err}
    if active>=maxActive{return TaskRecord{},nil,ErrTaskCapacity}
    var id,owner,revision,surface,tool,keyID,record string
    var nonce,ciphertext []byte
    err=tx.QueryRow(`SELECT q.task_id,q.owner_digest,q.revision,q.surface,q.tool,
        q.key_id,q.nonce,q.ciphertext,t.record
        FROM task_queue q JOIN tasks t ON t.task_id=q.task_id
        ORDER BY q.seq LIMIT 1`).Scan(&id,&owner,&revision,&surface,&tool,
        &keyID,&nonce,&ciphertext,&record)
    if err!=nil{return TaskRecord{},nil,err}
    var task TaskRecord
    if err=json.Unmarshal([]byte(record),&task);err!=nil{return TaskRecord{},nil,err}
    if task.TaskID!=id || task.OwnerDigest!=owner || task.Revision!=revision ||
        task.Surface!=surface || task.Tool!=tool ||
        task.Status!="queued" || task.Phase!="queued" {
        return TaskRecord{},nil,ErrTaskQueueKeyUnavailable
    }
    if task.TTLMS>0 && time.Since(task.CreatedAt)>time.Duration(task.TTLMS)*time.Millisecond {
        // Do not execute a request whose original authorization/TTL has
        // expired while it was waiting. Close the task and remove its
        // encrypted input in the same transaction.
        task.Status="failed"
        task.Phase=""
        task.StatusMessage="Queued task expired before execution"
        task.Error=map[string]any{"code":-32000,"message":"Queued task expired"}
        task.LastUpdatedAt=time.Now().UTC()
        expiredRaw,err:=json.Marshal(task)
        if err!=nil{return TaskRecord{},nil,err}
        if _,err=tx.Exec("UPDATE tasks SET status='failed',record=? WHERE task_id=? AND status='queued'",
            string(expiredRaw),id);err!=nil{return TaskRecord{},nil,err}
        if _,err=tx.Exec("DELETE FROM task_queue WHERE task_id=?",id);err!=nil{return TaskRecord{},nil,err}
        if err=tx.Commit();err!=nil{return TaskRecord{},nil,err}
        return TaskRecord{},nil,ErrTaskQueueExpired
    }
    args,err:=s.queueKeys.open(task,keyID,nonce,ciphertext)
    if err!=nil{return TaskRecord{},nil,err}
    task.Status="working"
    task.Phase=""
    task.StatusMessage="Executing queued pipeline"
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return TaskRecord{},nil,err}
    result,err:=tx.Exec(`UPDATE tasks SET status='working',record=?
        WHERE task_id=? AND status='queued'`,string(raw),id)
    if err!=nil{return TaskRecord{},nil,err}
    n,err:=result.RowsAffected()
    if err!=nil || n!=1{return TaskRecord{},nil,ErrTaskTransition}
    if _,err=tx.Exec("DELETE FROM task_queue WHERE task_id=?",id);err!=nil{return TaskRecord{},nil,err}
    if err=tx.Commit();err!=nil{return TaskRecord{},nil,err}
    return task,args,nil
}
func (s *Store) CancelQueuedTask(ctx context.Context,id,owner string)error {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return err}
    var record,status string
    if err=tx.QueryRow("SELECT record,status FROM tasks WHERE task_id=?",id).Scan(&record,&status);err!=nil{return err}
    var task TaskRecord
    if err=json.Unmarshal([]byte(record),&task);err!=nil{return err}
    if status!="queued" || task.Status!="queued" || task.OwnerDigest!=owner || task.Phase!="queued"{
        return ErrTaskTransition
    }
    task.Status="cancelled"
    task.Phase=""
    task.StatusMessage="Queued execution cancelled"
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return err}
    result,err:=tx.Exec("UPDATE tasks SET status='cancelled',record=? WHERE task_id=? AND status='queued'",string(raw),id)
    if err!=nil{return err}
    n,err:=result.RowsAffected()
    if err!=nil || n!=1{return ErrTaskTransition}
    if _,err=tx.Exec("DELETE FROM task_queue WHERE task_id=?",id);err!=nil{return err}
    return tx.Commit()
}
