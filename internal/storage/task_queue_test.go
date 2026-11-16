package storage

import (
    "bytes"
    "database/sql"
    "context"
    "encoding/json"
    "errors"
    "os"
    "path/filepath"
    "testing"
    "time"
)

func setQueueKeyRing(t *testing.T) {
    t.Helper()
    payload,_:=json.Marshal(map[string]any{
        "active":"k1",
        "keys":map[string]string{"k1":testGrantKey(t)},
    })
    t.Setenv(taskQueueKeyEnv,string(payload))
}
func queueStore(t *testing.T)*Store {
    t.Helper()
    setQueueKeyRing(t)
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){_ = s.Close()})
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    return s
}

func TestEncryptedQueueAdmissionFIFOAndCapacity(t *testing.T) {
    s:=queueStore(t)
    busy:=capacityTask(s,"working")
    queuedOne,queuedTwo:=capacityTask(s,"working"),capacityTask(s,"working")
    argsOne:=map[string]any{"secret":"alice-private-queue-input","seq":"first"}
    argsTwo:=map[string]any{"secret":"bob-private-queue-input","seq":"second"}
    q,err:=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{"id":0},1,2)
    if err!=nil || q {t.Fatalf("initial slot was queued: %v",err)}
    q,err=s.AdmitOrQueueTask(context.Background(),queuedOne,argsOne,1,2)
    if err!=nil || !q {t.Fatalf("first queue admission failed: %v",err)}
    q,err=s.AdmitOrQueueTask(context.Background(),queuedTwo,argsTwo,1,2)
    if err!=nil || !q {t.Fatalf("second queue admission failed: %v",err)}
    if _,err=s.AdmitOrQueueTask(context.Background(),capacityTask(s,"working"),
        map[string]any{"seq":"overflow"},1,2);!errors.Is(err,ErrTaskQueueFull) {
        t.Fatalf("bounded queue overflow admitted: %v",err)
    }
    if depth,err:=s.QueueDepth(context.Background());err!=nil || depth!=2 {
        t.Fatalf("queued work depth wrong: %d %v",depth,err)
    }
    waiting,err:=s.TaskGet(queuedOne.TaskID)
    if err!=nil || waiting.Status!="queued" || waiting.Public()["status"]!="working" ||
        waiting.Public()["statusMessage"]!="Waiting for available execution capacity" {
        t.Fatalf("queued task not MCP-compatible: %#v %v",waiting,err)
    }
    if _,_,err=s.ClaimNextQueued(context.Background(),1);!errors.Is(err,ErrTaskCapacity){
        t.Fatalf("claimed queue despite active capacity: %v",err)
    }
    backup:=filepath.Join(t.TempDir(),"tasks.sqlite")
    if err=s.Backup(backup);err!=nil{t.Fatal(err)}
    bytesOnDisk,err:=os.ReadFile(backup)
    if err!=nil{t.Fatal(err)}
    for _,secret:=range []string{"alice-private-queue-input","bob-private-queue-input"} {
        if bytes.Contains(bytesOnDisk,[]byte(secret)){t.Fatal("queued task plaintext leaked into SQLite backup")}
    }
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    first,input,err:=s.ClaimNextQueued(context.Background(),1)
    if err!=nil || first.TaskID!=queuedOne.TaskID || input["seq"]!="first" {
        t.Fatalf("FIFO task dequeue incorrect: %#v %#v %v",first,input,err)
    }
    if err=s.CancelQueuedTask(context.Background(),queuedTwo.TaskID,queuedTwo.OwnerDigest);err!=nil{
        t.Fatal(err)
    }
    if depth,err:=s.QueueDepth(context.Background());err!=nil || depth!=0 {
        t.Fatalf("cancellation retained ciphertext: %d %v",depth,err)
    }
    if _,_,err=s.ClaimNextQueued(context.Background(),2);!errors.Is(err,sql.ErrNoRows) {
        t.Fatal("cancelled queued task was reclaimed")
    }
}

func TestQueuedInputSurvivesRestartWithoutReplayingClaimedTask(t *testing.T) {
    setQueueKeyRing(t)
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=first.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    busy:=capacityTask(first,"working")
    if _,err=first.AdmitOrQueueTask(context.Background(),busy,map[string]any{"id":"busy"},1,2);err!=nil{t.Fatal(err)}
    pending:=capacityTask(first,"working")
    if _,err=first.AdmitOrQueueTask(context.Background(),pending,
        map[string]any{"id":"pending","input":"private"},1,2);err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    // An abandoned active task becomes failed; unclaimed sealed work remains
    // queued for the new execution leader and can be claimed exactly once.
    restarted,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer restarted.Close()
    if err=restarted.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    state,err:=restarted.TaskGet(busy.TaskID)
    if err!=nil || state.Status!="failed" {t.Fatalf("crashed running task replayed: %#v %v",state,err)}
    awaiting,err:=restarted.TaskGet(pending.TaskID)
    if err!=nil || awaiting.Status!="queued" {t.Fatalf("unclaimed work lost: %#v %v",awaiting,err)}
    retrieved,args,err:=restarted.ClaimNextQueued(context.Background(),1)
    if err!=nil || retrieved.TaskID!=pending.TaskID || args["id"]!="pending" {
        t.Fatalf("sealed queued input not restored: %#v %#v %v",retrieved,args,err)
    }
    if err=restarted.Close();err!=nil{t.Fatal(err)}
    third,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer third.Close()
    if err=third.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    crash,err:=third.TaskGet(pending.TaskID)
    if err!=nil || crash.Status!="failed" {
        t.Fatalf("claimed but interrupted external task was replayed: %#v %v",crash,err)
    }
    if n,err:=third.QueueDepth(context.Background());err!=nil || n!=0{
        t.Fatalf("claimed task restored to queue: %d %v",n,err)
    }
}

func TestQueueWithoutKeyFailsClosedAndRejectsOAuthDeferredWork(t *testing.T){
    t.Setenv(taskQueueKeyEnv,"")
    s:=capacityStore(t)
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{},1,2);err!=nil{t.Fatal(err)}
    waiting:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),waiting,map[string]any{"secret":"private"},1,2);
        !errors.Is(err,ErrTaskQueueKeyUnavailable) {t.Fatalf("plaintext queue admitted: %v",err)}
    if n,err:=s.QueueDepth(context.Background());err!=nil || n!=0 {
        t.Fatalf("unprotected queue row written: %d %v",n,err)
    }
    // With keys enabled a task requiring creation-time OAuth scopes cannot
    // have its bearer implicitly reused after expiration while queued.
    setQueueKeyRing(t)
    protected:=queueStore(t)
    worker:=capacityTask(protected,"working")
    if _,err:=protected.AdmitOrQueueTask(context.Background(),worker,map[string]any{},1,2);err!=nil{t.Fatal(err)}
    scoped:=capacityTask(protected,"working")
    scoped.RequiredScopes=[]string{"crm:read"}
    if _,err:=protected.AdmitOrQueueTask(context.Background(),scoped,map[string]any{"id":"secret"},1,2);
        !errors.Is(err,ErrTaskCapacity) {t.Fatalf("OAuth-scoped task queued without bearer: %v",err)}
}

func TestTaskQueueRejectsTamperingAndExpiredWork(t *testing.T){
    s:=queueStore(t)
    worker:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),worker,map[string]any{},1,3);err!=nil{t.Fatal(err)}
    expired:=capacityTask(s,"working")
    expired.CreatedAt=time.Now().Add(-time.Hour)
    expired.TTLMS=10
    if q,err:=s.AdmitOrQueueTask(context.Background(),expired,map[string]any{"foo":"bar"},1,3);
        err!=nil || !q {t.Fatalf("expired fixture admission failed: %v",err)}
    worker.Status="completed"
    if err:=s.TaskPut(worker);err!=nil{t.Fatal(err)}
    if _,_,err:=s.ClaimNextQueued(context.Background(),1);!errors.Is(err,ErrTaskQueueExpired){
        t.Fatalf("expired queued work dispatched: %v",err)
    }
    // TaskGet deliberately hides TTL-expired records. Inspect SQLite's
    // durable projection to prove the queued task was marked failed.
    var settled string
    if err:=s.db.QueryRow("SELECT status FROM tasks WHERE task_id=?",
        expired.TaskID).Scan(&settled);err!=nil || settled!="failed" {
        t.Fatalf("expired task not settled: %s %v",settled,err)
    }
    awaiting:=capacityTask(s,"working")
    busy:=capacityTask(s,"working")
    if _,err=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{},1,3);err!=nil{t.Fatal(err)}
    if _,err=s.AdmitOrQueueTask(context.Background(),awaiting,
        map[string]any{"secret":"protected"},1,3);err!=nil{t.Fatal(err)}
    if _,err=s.db.Exec("UPDATE task_queue SET revision=? WHERE task_id=?",
        "tampered-revision",awaiting.TaskID);err!=nil{t.Fatal(err)}
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    if _,_,err=s.ClaimNextQueued(context.Background(),1);!errors.Is(err,ErrTaskQueueKeyUnavailable){
        t.Fatalf("tampered queue metadata accepted: %v",err)
    }
    waiting,err:=s.TaskGet(awaiting.TaskID)
    if err!=nil || waiting.Status!="queued"{
        t.Fatalf("tampered queued data erased silently: %#v %v",waiting,err)
    }
}
