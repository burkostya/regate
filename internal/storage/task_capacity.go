package storage

import (
    "context"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "time"
)

// ErrTaskCapacity means no asynchronous execution slot is currently
// available. The caller must not create a task handle or dispatch external
// effects on rejection; this is explicit backpressure, not a hidden queue.
var ErrTaskCapacity = errors.New("asynchronous task concurrency limit reached")
var ErrTaskBacklog = errors.New("previously accepted queued tasks have admission precedence")
var ErrTaskTransition = errors.New("task cannot transition from its current state")

const DefaultMaxActiveTasks = 32
const MaxActiveTasksLimit = 1024

func validTaskLimit(limit int)error {
    if limit<1 || limit>MaxActiveTasksLimit {
        return fmt.Errorf("invalid active task limit")
    }
    return nil
}
func workingTaskCountTx(tx *sql.Tx)(int,error) {
    var n int
    err:=tx.QueryRow("SELECT COUNT(*) FROM tasks WHERE status='working'").Scan(&n)
    return n,err
}
func pendingTaskCountTx(tx *sql.Tx)(int,error) {
    var n int
    err:=tx.QueryRow("SELECT COUNT(*) FROM task_queue").Scan(&n)
    return n,err
}
func (s *Store) ActiveTaskCount(ctx context.Context)(int,error) {
    var n int
    err:=s.db.QueryRowContext(ctx,"SELECT COUNT(*) FROM tasks WHERE status='working'").Scan(&n)
    return n,err
}

// AdmitWorkingTask atomically reserves an active execution slot in the
// SQLite ledger BEFORE the goroutine starts. The same DB transaction checks
// the execution leader lease and counts existing active tasks, preventing
// competing server callers from oversubscribing even across connections.
func (s *Store) AdmitWorkingTask(ctx context.Context,task TaskRecord,limit int)error {
    if err:=validTaskLimit(limit);err!=nil{return err}
    if len(task.TaskID)!=24 || task.OwnerDigest=="" || task.Revision=="" ||
        task.Status!="working" || task.Phase!="" {
        return fmt.Errorf("invalid task for active admission")
    }
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return err}
    count,err:=workingTaskCountTx(tx)
    if err!=nil{return err}
    if count>=limit{return ErrTaskCapacity}
    if task.CreatedAt.IsZero(){task.CreatedAt=time.Now().UTC()}
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return err}
    _,err=tx.Exec("INSERT INTO tasks(task_id,status,record) VALUES(?,?,?)",
        task.TaskID,"working",string(raw))
    if err!=nil{return err}
    return tx.Commit()
}

// ClaimTaskContinuation is an atomic status-CAS from input_required to
// working with a capacity reservation. Unlike TaskPut, it cannot overwrite
// a concurrent cancellation, already running task or another continuation.
func (s *Store) ClaimTaskContinuation(ctx context.Context,task TaskRecord,limit int)error {
    if err:=validTaskLimit(limit);err!=nil{return err}
    if len(task.TaskID)!=24 || task.Status!="working" || task.Phase!="" {
        return ErrTaskTransition
    }
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return err}
    var priorRaw,status string
    if err=tx.QueryRow("SELECT record,status FROM tasks WHERE task_id=?",task.TaskID).
        Scan(&priorRaw,&status);err!=nil{return err}
    var prior TaskRecord
    if err=json.Unmarshal([]byte(priorRaw),&prior);err!=nil{return err}
    if status!="input_required" || prior.Status!=status ||
        prior.TaskID!=task.TaskID || prior.OwnerDigest!=task.OwnerDigest ||
        prior.Revision!=task.Revision || prior.Surface!=task.Surface ||
        prior.Tool!=task.Tool || prior.Phase!="" {
        return ErrTaskTransition
    }
    count,err:=workingTaskCountTx(tx)
    if err!=nil{return err}
    if count>=limit{return ErrTaskCapacity}
    // A resumed checkpoint must not jump ahead of encrypted jobs that were
    // admitted while this task awaited user input. Both checks share the
    // same SQLite write transaction as its status-CAS.
    backlog,err:=pendingTaskCountTx(tx)
    if err!=nil{return err}
    if backlog>0{return ErrTaskBacklog}
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return err}
    result,err:=tx.Exec(`UPDATE tasks SET status='working',record=?
        WHERE task_id=? AND status='input_required'`,string(raw),task.TaskID)
    if err!=nil{return err}
    affected,err:=result.RowsAffected()
    if err!=nil || affected!=1{return ErrTaskTransition}
    return tx.Commit()
}
