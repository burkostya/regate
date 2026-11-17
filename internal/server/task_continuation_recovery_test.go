package server

import (
    "context"
    "database/sql"
    "strings"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/engine"
    "github.com/burkostya/regate/internal/storage"
)

const taskContinuationRecoverySource = `pipeline "approval" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "wait" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  node "done" {
    kind = "output"
    value = "nodes.wait.value"
  }
  edge {
    from = "entry"
    to = "wait"
  }
  edge {
    from = "wait"
    to = "done"
  }
}
surface "approval" {
  path = "/mcp/approval"
  tool "approve" {
    pipeline = "approval"
    read_only = true
    output_schema = {type = "string"}
  }
}
`

func waitTaskRecoveryState(t *testing.T, svc *Service, id, status, message string) storage.TaskRecord {
    t.Helper()
    deadline := time.Now().Add(5*time.Second)
    for time.Now().Before(deadline) {
        task, err := svc.Store.TaskGet(id)
        if err != nil { t.Fatal(err) }
        if task.Status == status && (message == "" || task.StatusMessage == message) {
            return task
        }
        time.Sleep(10*time.Millisecond)
    }
    current, err := svc.Store.TaskGet(id)
    t.Fatalf("task did not reach %s/%q: %#v %v",status,message,current,err)
    return storage.TaskRecord{}
}

func startRecoverableTask(t *testing.T, source string) (*Service, string, storage.TaskRecord) {
    t.Helper()
    svc, err := New(t.TempDir(),source)
    if err != nil {t.Fatal(err)}
    t.Cleanup(func(){_ = svc.Store.Close()})
    created, err := svc.CreateTask(context.Background(),"approval","approve","owner",map[string]any{})
    if err != nil {t.Fatal(err)}
    id, ok := created["taskId"].(string)
    if !ok || id == "" {t.Fatalf("missing task id: %#v",created)}
    task := waitTaskRecoveryState(t,svc,id,"input_required","Agent or user input needed to resume pipeline")
    if task.RunID == "" {t.Fatal("suspended task missing run identity")}
    return svc,id,task
}

func TestRejectedContinuationLeavesAsyncTaskCorrectable(t *testing.T) {
    svc,id,initial:=startRecoverableTask(t,taskContinuationRecoverySource)
    if _,err:=svc.TaskUpdate(id,"owner",map[string]any{"resume":map[string]any{
        "action":"accept","content":42,
    }});err!=nil {t.Fatal(err)}
    rejected:=waitTaskRecoveryState(t,svc,id,"input_required",
        "Continuation rejected; checkpoint retained. Check external effects before retrying.")
    if rejected.RunID!=initial.RunID || rejected.Result!=nil ||
        rejected.InputRequests==nil {
        t.Fatalf("rejected continuation lost resumability: %#v",rejected)
    }
    var cp engine.Checkpoint
    if err:=svc.Store.GetSuspended(initial.RunID,&cp);err!=nil ||
        cp.States["wait"]!="pending" {
        t.Fatalf("invalid leaf answer corrupted durable checkpoint: %#v %v",cp,err)
    }
    if _,err:=svc.TaskUpdate(id,"owner",map[string]any{"resume":map[string]any{
        "action":"accept","content":"approved",
    }});err!=nil {t.Fatal(err)}
    finished:=waitTaskRecoveryState(t,svc,id,"completed","Execution completed")
    if finished.Result==nil || finished.Result["isError"]!=false ||
        finished.Result["structuredContent"]!="approved" {
        t.Fatalf("valid correction did not complete task: %#v",finished)
    }
    if err:=svc.Store.GetSuspended(initial.RunID,&cp);err!=sql.ErrNoRows {
        t.Fatalf("completed run retained suspended checkpoint: %v",err)
    }
}

func TestFailedFinalResponseConsumesAsyncCheckpoint(t *testing.T) {
    // Differentiate an operator-correctable leaf schema rejection from
    // completed graph failure at the *published* tool output boundary.
    source:=strings.Replace(taskContinuationRecoverySource,
        `    output_schema = {type = "string"}
  }
}
`,`    output_schema = {type = "string", pattern = "^approved$"}
  }
}
`,1)
    if source==taskContinuationRecoverySource {t.Fatal("fixture replacement failed")}
    svc,id,initial:=startRecoverableTask(t,source)
    if _,err:=svc.TaskUpdate(id,"owner",map[string]any{"resume":map[string]any{
        "action":"accept","content":"rejected",
    }});err!=nil {t.Fatal(err)}
    finished:=waitTaskRecoveryState(t,svc,id,"completed","Execution completed")
    if finished.Result==nil || finished.Result["isError"]!=true ||
        finished.InputRequests!=nil {
        t.Fatalf("final response failure incorrectly presented as resumable: %#v",finished)
    }
    var cp engine.Checkpoint
    if err:=svc.Store.GetSuspended(initial.RunID,&cp);err!=sql.ErrNoRows {
        t.Fatalf("consumed graph checkpoint must not be retryable: %v",err)
    }
}
