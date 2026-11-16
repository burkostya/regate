package storage

import (
    "context"
    "errors"
    "testing"
    "time"
)

func expiredQueuedTask(s *Store,owner string)TaskRecord {
    task:=capacityTask(s,"working")
    task.OwnerDigest=owner
    task.CreatedAt=time.Now().Add(-time.Hour)
    task.TTLMS=10
    return task
}

func queuedTaskSQLStatus(t *testing.T,s *Store,id string)string {
    t.Helper()
    var status string
    if err:=s.db.QueryRow("SELECT status FROM tasks WHERE task_id=?",id).
        Scan(&status);err!=nil{t.Fatal(err)}
    return status
}

func TestQueueExpirySweepReleasesBacklogDespiteFullActiveCapacity(t *testing.T) {
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{"id":"busy"},1,4);
        err!=nil{t.Fatal(err)}
    expired:=expiredQueuedTask(s,"owner-expired")
    if queued,err:=s.AdmitOrQueueTask(ctx,expired,map[string]any{"id":"old"},1,4);
        err!=nil || !queued{t.Fatalf("expired fixture was not queued: %v",err)}
    fresh:=capacityTask(s,"working")
    fresh.OwnerDigest="owner-fresh"
    if queued,err:=s.AdmitOrQueueTask(ctx,fresh,map[string]any{"id":"fresh"},1,4);
        err!=nil || !queued{t.Fatalf("fresh queue admission: %v",err)}
    if active,err:=s.ActiveTaskCount(ctx);err!=nil || active!=1{
        t.Fatalf("running worker not saturated: %d %v",active,err)
    }
    removed,err:=s.SweepExpiredQueued(ctx,64)
    if err!=nil || removed!=1{
        t.Fatalf("expiry sweep failed under full execution: %d %v",removed,err)
    }
    if queuedTaskSQLStatus(t,s,expired.TaskID)!="failed"{
        t.Fatal("expired queued task not durably failed")
    }
    if depth,err:=s.QueueDepth(ctx);err!=nil || depth!=1{
        t.Fatalf("expired ciphertext still occupies queue: %d %v",depth,err)
    }
    if active,err:=s.ActiveTaskCount(ctx);err!=nil || active!=1{
        t.Fatalf("expiry cleanup accidentally dispatched work: %d %v",active,err)
    }
    accepted:=capacityTask(s,"working")
    if queued,err:=s.AdmitOrQueueTask(ctx,accepted,map[string]any{"id":"new"},1,2);
        err!=nil || !queued {
        t.Fatalf("expired task prevented bounded new admission: %v",err)
    }
}

func TestQueueExpirySweepBoundedAndLeaseFenced(t *testing.T) {
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{},1,4);err!=nil{
        t.Fatal(err)
    }
    one:=expiredQueuedTask(s,"owner-1")
    two:=expiredQueuedTask(s,"owner-2")
    for _,task:=range []TaskRecord{one,two}{
        if queued,err:=s.AdmitOrQueueTask(ctx,task,map[string]any{"id":"old"},1,4);
            err!=nil || !queued {t.Fatalf("cannot stage expired queue: %v",err)}
    }
    if _,err:=s.SweepExpiredQueued(ctx,0);err==nil{
        t.Fatal("unbounded zero-size sweep accepted")
    }
    if _,err:=s.SweepExpiredQueued(ctx,maxQueuedExpirySweep+1);err==nil{
        t.Fatal("overly broad expiry sweep accepted")
    }
    count,err:=s.SweepExpiredQueued(ctx,1)
    if err!=nil || count!=1{t.Fatalf("bounded sweep failed: %d %v",count,err)}
    if depth,err:=s.QueueDepth(ctx);err!=nil || depth!=1{
        t.Fatalf("bounded sweep removed more than one job: %d %v",depth,err)
    }
    observer,err:=New(s.Dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer observer.Close()
    if _,err=observer.SweepExpiredQueued(ctx,64);
        !errors.Is(err,ErrExecutionLeaseHeld) {
        t.Fatalf("observer deleted leader's queued tasks: %v",err)
    }
    count,err=s.SweepExpiredQueued(ctx,64)
    if err!=nil || count!=1{t.Fatalf("remaining expired row not removed: %d %v",count,err)}
}

func TestQueueExpirySweepCorruptionRollsBackWholeBatch(t *testing.T){
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{},1,3);err!=nil{t.Fatal(err)}
    first:=expiredQueuedTask(s,"owner-A")
    second:=expiredQueuedTask(s,"owner-B")
    for _,task:=range []TaskRecord{first,second}{
        if _,err:=s.AdmitOrQueueTask(ctx,task,map[string]any{"sensitive":"stored"},1,3);
            err!=nil{t.Fatal(err)}
    }
    if _,err:=s.db.Exec("UPDATE task_queue SET ciphertext=? WHERE task_id=?",
        []byte("invalid-ciphertext"),second.TaskID);err!=nil{t.Fatal(err)}
    if _,err:=s.SweepExpiredQueued(ctx,64);
        !errors.Is(err,ErrTaskQueueKeyUnavailable){
        t.Fatalf("tampered expired input was discarded: %v",err)
    }
    if queuedTaskSQLStatus(t,s,first.TaskID)!="queued" ||
        queuedTaskSQLStatus(t,s,second.TaskID)!="queued" {
        t.Fatal("expiry sweep partially deleted authenticated evidence")
    }
    if depth,err:=s.QueueDepth(ctx);err!=nil || depth!=2 {
        t.Fatalf("atomic sweep lost rows: %d %v",depth,err)
    }
}

func TestQueueAdmissionRequiresBoundedRetentionForEncryptedPayload(t *testing.T){
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{},1,3);err!=nil{t.Fatal(err)}
    for _,ttl:=range []int64{0,-1,maxQueueTTLMS+1}{
        job:=capacityTask(s,"working")
        job.TTLMS=ttl
        if _,err:=s.AdmitOrQueueTask(ctx,job,map[string]any{"private":"payload"},1,3);
            !errors.Is(err,ErrTaskQueueExpired){
            t.Fatalf("unbounded encrypted retention accepted (TTL %d): %v",ttl,err)
        }
    }
    if depth,err:=s.QueueDepth(ctx);err!=nil || depth!=0 {
        t.Fatalf("invalid TTL inserted queued input: %d %v",depth,err)
    }
}

func TestExpirySweepReleasesOwnerSchedulingMetadata(t *testing.T) {
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{},1,4);err!=nil{t.Fatal(err)}
    first:=capacityTask(s,"working")
    first.OwnerDigest="owner-A"
    last:=expiredQueuedTask(s,"owner-A")
    for _,task:=range []TaskRecord{first,last}{
        if _,err:=s.AdmitOrQueueTask(ctx,task,map[string]any{"id":"owner-A"},1,4);
            err!=nil{t.Fatal(err)}
    }
    busy.Status="completed"
    if err:=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    processed,_,err:=s.ClaimNextQueued(ctx,1)
    if err!=nil || processed.TaskID!=first.TaskID {
        t.Fatalf("expected first owner-A job to earn turn: %#v %v",processed,err)
    }
    processed.Status="completed"
    if err=s.TaskPut(processed);err!=nil{t.Fatal(err)}
    var before int
    if err=s.db.QueryRow("SELECT COUNT(*) FROM task_queue_turns WHERE owner_digest=?",
        "owner-A").Scan(&before);err!=nil || before!=1 {
        t.Fatalf("owner turn not persisted: %d %v",before,err)
    }
    if n,err:=s.SweepExpiredQueued(ctx,64);err!=nil || n!=1 {
        t.Fatalf("expired last owner task not cleaned: %d %v",n,err)
    }
    var after int
    if err=s.db.QueryRow("SELECT COUNT(*) FROM task_queue_turns WHERE owner_digest=?",
        "owner-A").Scan(&after);err!=nil || after!=0 {
        t.Fatalf("orphan fairness turn was not cleaned: %d %v",after,err)
    }
}
