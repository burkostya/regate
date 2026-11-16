package storage

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "os"
    "path/filepath"
    "testing"
)

func setQueueRotationRing(t *testing.T,active string,keys map[string]string) {
    t.Helper()
    encoded,err:=json.Marshal(map[string]any{"active":active,"keys":keys})
    if err!=nil{t.Fatal(err)}
    t.Setenv(taskQueueKeyEnv,string(encoded))
}
func queueRotationFixture(t *testing.T,dir string)(*Store,[]TaskRecord) {
    t.Helper()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    busy:=capacityTask(s,"working")
    if _,err=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{"id":"busy"},1,5);err!=nil{
        t.Fatal(err)
    }
    entries:=make([]TaskRecord,0,3)
    for i:=0;i<3;i++ {
        task:=capacityTask(s,"working")
        admitted,err:=s.AdmitOrQueueTask(context.Background(),task,
            map[string]any{"position":i,"secret":"encrypted-queue-"+string(rune('a'+i))},
            1,5)
        if err!=nil || !admitted {t.Fatalf("queue %d: %v",i,err)}
        entries=append(entries,task)
    }
    return s,entries
}

func TestQueueKeyRewrapAllowsRetiringOldKeyAfterRestart(t *testing.T) {
    key1,key2:=testGrantKey(t),testGrantKey(t)
    setQueueRotationRing(t,"k1",map[string]string{"k1":key1})
    dir:=t.TempDir()
    first,items:=queueRotationFixture(t,dir)
    if err:=first.Close();err!=nil{t.Fatal(err)}

    setQueueRotationRing(t,"k2",map[string]string{"k1":key1,"k2":key2})
    second,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=second.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    ctx:=context.Background()
    before,err:=second.QueueKeyUsage(ctx)
    if err!=nil || before.ActiveKey!="k2" || before.ByKey["k1"]!=3 {
        t.Fatalf("queued key usage not reported: %#v %v",before,err)
    }
    report,err:=second.RewrapQueuedBatch(ctx,2)
    if err!=nil || report.Rewrapped!=2 ||
        report.RemainingOldKey!=1 || report.ByKey["k2"]!=2 {
        t.Fatalf("bounded key rewrap incorrect: %#v %v",report,err)
    }
    report,err=second.RewrapQueuedBatch(ctx,2)
    if err!=nil || report.Rewrapped!=1 ||
        report.RemainingOldKey!=0 || report.ByKey["k2"]!=3 {
        t.Fatalf("final key rewrap incorrect: %#v %v",report,err)
    }
    report,err=second.RewrapQueuedBatch(ctx,2)
    if err!=nil || report.Rewrapped!=0 {
        t.Fatalf("no-op rewrap should not mutate: %#v %v",report,err)
    }
    snapshot:=filepath.Join(t.TempDir(),"rotated.sqlite")
    if err=second.Backup(snapshot);err!=nil{t.Fatal(err)}
    data,err:=os.ReadFile(snapshot)
    if err!=nil{t.Fatal(err)}
    if bytes.Contains(data,[]byte("encrypted-queue-a")){
        t.Fatal("plaintext was persisted during queued key rotation")
    }
    if err=second.Close();err!=nil{t.Fatal(err)}

    // The removed legacy key is no longer available, yet all queued input
    // remains fully authenticated and recoverable with k2 alone.
    setQueueRotationRing(t,"k2",map[string]string{"k2":key2})
    reopened,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer reopened.Close()
    if err=reopened.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    for i,item:=range items {
        claimed,args,err:=reopened.ClaimNextQueued(ctx,1)
        if err!=nil || claimed.TaskID!=item.TaskID ||
            args["position"]!=float64(i) {
            t.Fatalf("rotated item %d unreadable: %#v %#v %v",i,claimed,args,err)
        }
        claimed.Status="completed"
        if err=reopened.TaskPut(claimed);err!=nil{t.Fatal(err)}
    }
    after,err:=reopened.QueueKeyUsage(ctx)
    if err!=nil || after.Total!=0 {t.Fatalf("queue did not drain: %#v %v",after,err)}
}

func TestQueueRewrapCorruptionIsAtomicAndPreservesOriginalEncryptedRows(t *testing.T){
    key1,key2:=testGrantKey(t),testGrantKey(t)
    setQueueRotationRing(t,"k1",map[string]string{"k1":key1})
    dir:=t.TempDir()
    original,items:=queueRotationFixture(t,dir)
    if _,err:=original.db.Exec("UPDATE task_queue SET ciphertext=? WHERE task_id=?",
        []byte("invalid-ciphertext"),items[1].TaskID);err!=nil{t.Fatal(err)}
    if err:=original.Close();err!=nil{t.Fatal(err)}
    setQueueRotationRing(t,"k2",map[string]string{"k1":key1,"k2":key2})
    restored,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer restored.Close()
    if err=restored.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    ctx:=context.Background()
    if _,err=restored.RewrapQueuedBatch(ctx,3);
        !errors.Is(err,ErrTaskQueueKeyUnavailable) {
        t.Fatalf("corrupt ciphertext was accepted: %v",err)
    }
    usage,err:=restored.QueueKeyUsage(ctx)
    if err!=nil || usage.ByKey["k1"]!=3 || usage.ByKey["k2"]!=0{
        t.Fatalf("partially committed failed rewrap: %#v %v",usage,err)
    }
    if _,err=restored.RewrapQueuedBatch(ctx,0);err==nil{
        t.Fatal("zero-sized rewrap was allowed")
    }
    if _,err=restored.RewrapQueuedBatch(ctx,65);err==nil{
        t.Fatal("unbounded rewrap was allowed")
    }
}
func TestQueueRewrapRequiresCurrentExecutionLeader(t *testing.T){
    key1,key2:=testGrantKey(t),testGrantKey(t)
    setQueueRotationRing(t,"k1",map[string]string{"k1":key1})
    dir:=t.TempDir()
    leader,_:=queueRotationFixture(t,dir)
    defer leader.Close()
    setQueueRotationRing(t,"k2",map[string]string{"k1":key1,"k2":key2})
    observer,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer observer.Close()
    if _,err=observer.RewrapQueuedBatch(context.Background(),1);
        !errors.Is(err,ErrExecutionLeaseHeld) {
        t.Fatalf("observer rewrapped active worker queue: %v",err)
    }
}
func TestQueueKeyUsageNeverExportsMaterial(t *testing.T) {
    key:=testGrantKey(t)
    setQueueRotationRing(t,"k1",map[string]string{"k1":key})
    dir:=t.TempDir()
    store,items:=queueRotationFixture(t,dir)
    defer store.Close()
    usage,err:=store.QueueKeyUsage(context.Background())
    if err!=nil || usage.ActiveKey!="k1" || usage.Total!=len(items) {
        t.Fatalf("incorrect key usage projection: %#v %v",usage,err)
    }
    raw,err:=json.Marshal(usage)
    if err!=nil{t.Fatal(err)}
    if bytes.Contains(raw,[]byte(key)) ||
        bytes.Contains(raw,[]byte("encrypted-queue")) ||
        bytes.Contains(raw,[]byte(items[0].TaskID)) {
        t.Fatal("sensitive queued content leaked through key-usage projection")
    }
}
