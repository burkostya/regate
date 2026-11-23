package storage

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "time"
)

// DelegationRequirement persists only the identity and provider trust
// boundary. It does NOT contain an access token, refresh token or secret. A
// suspended delegated task requires a newly verified bearer on each resume.
type DelegationRequirement struct {
    Provider string `json:"provider"`
    Issuer string `json:"issuer"`
    Audience string `json:"audience"`
}

// TaskRecord is durable. OwnerDigest, authorization scopes, delegation trust
// boundary and the persisted execution revision never appear in Public.
type TaskRecord struct {
    TaskID string `json:"taskId"`
    Status string `json:"status"`
    StatusMessage string `json:"statusMessage,omitempty"`
    // Internal continuation barrier, never exposed by Public.
    Phase string `json:"execution_phase,omitempty"`
    OwnerDigest string `json:"owner_digest"`
    Surface string `json:"surface"`
    Tool string `json:"tool,omitempty"`
    RequiredScopes []string `json:"required_scopes,omitempty"`
    Delegation *DelegationRequirement `json:"delegation,omitempty"`
    RedactPII bool `json:"redact_pii,omitempty"`
    RunID string `json:"run_id,omitempty"`
    ToolCallObserved bool `json:"tool_call_observed,omitempty"`
    Revision string `json:"revision"`
    CreatedAt time.Time `json:"createdAt"`
    LastUpdatedAt time.Time `json:"lastUpdatedAt"`
    TTLMS int64 `json:"ttlMs"`
    PollIntervalMS int64 `json:"pollIntervalMs"`
    Result map[string]any `json:"result,omitempty"`
    Error map[string]any `json:"error,omitempty"`
    InputRequests map[string]any `json:"inputRequests,omitempty"`
}
func (s *Store) TaskPut(task TaskRecord) error {
    if len(task.TaskID)!=24 || task.OwnerDigest=="" || task.Revision=="" || task.Status=="" {
        return fmt.Errorf("invalid task identity or state")
    }
    if task.CreatedAt.IsZero(){task.CreatedAt=time.Now().UTC()}
    task.LastUpdatedAt=time.Now().UTC()
    raw,err:=json.Marshal(task)
    if err!=nil{return err}
    _,err=s.db.Exec(`INSERT INTO tasks(task_id,status,record) VALUES(?,?,?)
        ON CONFLICT(task_id) DO UPDATE SET
        status=excluded.status,record=excluded.record`,
        task.TaskID,task.Status,string(raw))
    return err
}
func (s *Store) TaskGet(id string)(TaskRecord,error) {
    if len(id)!=24{return TaskRecord{},fmt.Errorf("invalid task id")}
    var raw string
    err:=s.db.QueryRow("SELECT record FROM tasks WHERE task_id=?",id).Scan(&raw)
    if err!=nil{return TaskRecord{},err}
    var task TaskRecord
    if err=json.Unmarshal([]byte(raw),&task);err!=nil{return TaskRecord{},err}
    if task.TTLMS>0 && time.Since(task.CreatedAt)>time.Duration(task.TTLMS)*time.Millisecond {
        return TaskRecord{},fmt.Errorf("task expired")
    }
    return task,nil
}
func (t TaskRecord) Public() map[string]any {
    publicStatus:=t.Status
    if publicStatus=="queued" {publicStatus="working"} // MCP task status compatibility
    out:=map[string]any{
        "taskId":t.TaskID,"status":publicStatus,
        "createdAt":t.CreatedAt.Format(time.RFC3339Nano),
        "lastUpdatedAt":t.LastUpdatedAt.Format(time.RFC3339Nano),
        "ttlMs":t.TTLMS,"pollIntervalMs":t.PollIntervalMS,
    }
    if t.StatusMessage!=""{out["statusMessage"]=t.StatusMessage}
    if t.Status=="completed" && t.Result!=nil {out["result"]=t.Result}
    if t.Status=="failed" && t.Error!=nil {out["error"]=t.Error}
    if t.Status=="input_required" && t.InputRequests!=nil {out["inputRequests"]=t.InputRequests}
    return out
}

// recoverInterruptedTasksTx terminates orphaned tasks without replaying them.
// Callers must establish in the same transaction that no previous lease is live.
func recoverInterruptedTasksTx(tx *sql.Tx) error {
    rows,err:=tx.Query("SELECT task_id,record FROM tasks WHERE status='working'")
    if err!=nil{return err}
    tasks:=[]TaskRecord{}
    for rows.Next() {
        var id,raw string
        if err=rows.Scan(&id,&raw);err!=nil{break}
        var task TaskRecord
        if err=json.Unmarshal([]byte(raw),&task);err!=nil{break}
        if task.TaskID!=id {err=fmt.Errorf("corrupt task ID in SQLite");break}
        tasks=append(tasks,task)
    }
    if readErr:=rows.Err();err==nil {err=readErr}
    if closeErr:=rows.Close();err==nil{err=closeErr}
    if err!=nil{return err}
    for _,task:=range tasks {
        task.Status="failed"
        task.StatusMessage="Server restarted: reconcile external effects before retry"
        task.Error=map[string]any{
            "code":-32000,
            "message":"Interrupted execution; external effect outcomes require reconciliation",
        }
        task.LastUpdatedAt=time.Now().UTC()
        raw,err:=json.Marshal(task)
        if err!=nil{return err}
        result,err:=tx.Exec(`UPDATE tasks SET status='failed',record=?
            WHERE task_id=? AND status='working'`,string(raw),task.TaskID)
        if err!=nil{return err}
        affected,err:=result.RowsAffected()
        if err!=nil{return err}
        if affected!=1 {return sql.ErrNoRows}
    }
    return nil
}
