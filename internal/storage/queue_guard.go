package storage

import (
    "context"
    "database/sql"
    "encoding/json"
)

// RequireWorkingTask is the runtime authorization barrier for dispatched
// queued work. A cancelled, failed, expired, wrong-owner or replaced task
// cannot start another MCP/HTTP effect, even if the goroutine was admitted
// just before cancellation.
func (s *Store) RequireWorkingTask(ctx context.Context,taskID,owner string)error {
    if err:=s.RequireExecutionLeader(ctx);err!=nil{return err}
    var record,status string
    err:=s.db.QueryRowContext(ctx,
        "SELECT record,status FROM tasks WHERE task_id=?",taskID).Scan(&record,&status)
    if err!=nil {if err==sql.ErrNoRows{return ErrTaskTransition};return err}
    var task TaskRecord
    if err=json.Unmarshal([]byte(record),&task);err!=nil{return err}
    if task.TaskID!=taskID || task.OwnerDigest!=owner ||
        task.Status!="working" || status!="working" || task.Phase!="" ||
        task.Delegation!=nil || len(task.RequiredScopes)>0 {
        return ErrTaskTransition
    }
    return nil
}
