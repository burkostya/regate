package server

import (
    "database/sql"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/engine"
)

func TestAsyncTaskCanDrainCommittedFinalAnswerAfterCrash(t *testing.T) {
    svc, id, initial := startRecoverableTask(t, taskContinuationRecoverySource)
    var cp engine.Checkpoint
    if err := svc.Store.GetSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    // Simulate the process dying after SQLite accepted the final reply but
    // before execution of the downstream graph. No new answer is permitted.
    cp.States["wait"] = "completed"
    cp.Values["wait"] = map[string]any{"value":"durable","status":"success"}
    cp.Fired["wait>done"] = true
    if err := svc.Store.PutSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    for _, response := range []map[string]any{
        {"action":"accept","content":"replacement"},
        {"action":"accept","recover":true,"content":nil},
        {"action":"accept","recover":true,"path":[]any{"wait"}},
        {"action":"accept","recover":false},
    } {
        _, err := svc.TaskUpdate(id, "owner", map[string]any{"resume":response})
        if err == nil || !strings.Contains(err.Error(),"recovery requires") {
            t.Fatalf("duplicate answer accepted in recovery: %#v %v",response,err)
        }
    }
    if _, err := svc.TaskUpdate(id, "other-owner", map[string]any{
        "resume":map[string]any{"action":"accept","recover":true},
    }); err == nil { t.Fatal("wrong owner recovered the continuation") }
    if _, err := svc.TaskUpdate(id, "owner", map[string]any{
        "resume":map[string]any{"action":"accept","recover":true},
    }); err != nil { t.Fatalf("explicit recovery rejected: %v",err) }

    finished := waitTaskRecoveryState(t,svc,id,"completed","Execution completed")
    if finished.Result == nil || finished.Result["isError"] != false ||
        finished.Result["structuredContent"] != "durable" {
        t.Fatalf("answer-free recovery lost accepted result: %#v",finished)
    }
    if err := svc.Store.GetSuspended(initial.RunID,&cp); err != sql.ErrNoRows {
        t.Fatalf("completed recovered run retained checkpoint: %v",err)
    }
}

func TestAsyncDownstreamFailureRequestsRecoveryNotDuplicateInput(t *testing.T) {
    source := strings.Replace(taskContinuationRecoverySource,
        `value = "nodes.wait.value"`, `value = "nodes.wait.value.nonexistent"`, 1)
    if source == taskContinuationRecoverySource { t.Fatal("fixture changed") }
    svc,id,initial := startRecoverableTask(t,source)
    if _,err:=svc.TaskUpdate(id,"owner",map[string]any{
        "resume":map[string]any{"action":"accept","content":"approved"},
    });err!=nil { t.Fatal(err) }
    retained:=waitTaskRecoveryState(t,svc,id,"input_required",
        "Accepted answer retained; verify external effects before recovery.")
    if len(retained.InputRequests) != 1 {
        t.Fatalf("expected one recovery request: %#v", retained.InputRequests)
    }
    var recoveryKey string
    for key, entry := range retained.InputRequests {
        request, ok := entry.(map[string]any)
        if !ok || request["recovery"] != true || !strings.HasPrefix(key, "resume-") {
            t.Fatalf("task solicits replacement answer instead of recovery: %#v", retained.InputRequests)
        }
        recoveryKey = key
    }
    var cp engine.Checkpoint
    if err:=svc.Store.GetSuspended(initial.RunID,&cp);err!=nil {t.Fatal(err)}
    if cp.States["wait"]!="completed" || len(engine.PendingSuspensionPaths(&cp))!=0 {
        t.Fatalf("accepted final continuation was not stored: %#v",cp)
    }
    if _,err:=svc.TaskUpdate(id,"owner",map[string]any{
        recoveryKey:map[string]any{"action":"accept","content":"replace"},
    });err==nil || !strings.Contains(err.Error(),"recovery requires") {
        t.Fatalf("replacement answer unexpectedly accepted: %v",err)
    }
}
