package storage

import (
    "context"
    "errors"
    "testing"
    "time"
)

func TestSQLiteExecutionLeaseRejectsSecondActiveWorker(t *testing.T) {
    dir:=t.TempDir()
    leader,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer leader.Close()
    if err=leader.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    if err=leader.CheckExecutionLease(context.Background());err!=nil{t.Fatal(err)}
    observer,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer observer.Close()
    if err=observer.AcquireExecutionLease();!errors.Is(err,ErrExecutionLeaseHeld){
        t.Fatalf("second live worker admitted: %v",err)
    }
    if err=observer.CheckExecutionLease(context.Background());!errors.Is(err,ErrExecutionLeaseHeld){
        t.Fatalf("observer allowed external effect: %v",err)
    }
    if _,err=observer.BeginEffect("run-b","create","crm.create",map[string]any{"id":1});!errors.Is(err,ErrExecutionLeaseHeld){
        t.Fatalf("observer admitted a write: %v",err)
    }
    // Retain queued work; opening an observer must not fail an active worker's
    // task simply because they access the same SQLite file.
    rev,_,err:=leader.Active()
    if err!=nil{t.Fatal(err)}
    task:=TaskRecord{TaskID:ID(),Status:"working",OwnerDigest:"owner",
        Surface:"support",Revision:rev.ID,TTLMS:3600000}
    if err=leader.TaskPut(task);err!=nil{t.Fatal(err)}
    pending,err:=leader.BeginEffect("live-run","create","crm.create",map[string]any{"id":3})
    if err!=nil{t.Fatal(err)}
    // The first observer opened before TaskPut. A fresh observer still cannot
    // run automatic recovery while this leader is alive.
    third,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer third.Close()
    current,err:=third.TaskGet(task.TaskID)
    if err!=nil || current.Status!="working" {
        t.Fatalf("live task marked interrupted by observer: %#v %v",current,err)
    }
    if err=leader.Close();err!=nil{t.Fatal(err)}
    if err=observer.AcquireExecutionLease();err!=nil {
        t.Fatalf("released execution lease could not be acquired: %v",err)
    }
    if err=observer.CheckExecutionLease(context.Background());err!=nil {t.Fatal(err)}
    // Acquisition itself recovers orphaned work atomically under its new
    // fencing epoch, even though this observer opened before the old
    // leader released the lease.
    recovered,err:=observer.TaskGet(task.TaskID)
    if err!=nil || recovered.Status!="failed" {t.Fatalf("takeover recovery incorrect: %#v %v",recovered,err)}
    effect,err:=observer.EffectGet(pending.ID)
    if err!=nil || effect.Status!="unknown" {
        t.Fatalf("abandoned dispatch was not marked unknown during takeover: %#v %v",effect,err)
    }
}

func TestSQLiteFencingEpochRejectsOldWorkerAfterTakeover(t *testing.T) {
    dir:=t.TempDir()
    old,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer old.Close()
    if err=old.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    old.mu.RLock()
    oldEpoch:=old.leaseEpoch
    old.mu.RUnlock()

    // Force a lease deadline expiration without sleeping 30 seconds, as if
    // a process lost network/disk access and stopped heartbeating.
    if _,err=old.db.Exec("UPDATE execution_leases SET deadline_ns=? WHERE name=?",time.Now().Add(-time.Second).UnixNano(),executionLeaseName);err!=nil{
        t.Fatal(err)
    }
    next,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer next.Close()
    if err=next.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    next.mu.RLock()
    newEpoch:=next.leaseEpoch
    next.mu.RUnlock()
    if newEpoch<=oldEpoch {t.Fatalf("fencing epoch did not advance: %d -> %d",oldEpoch,newEpoch)}
    if err=old.CheckExecutionLease(context.Background());!errors.Is(err,ErrExecutionLeaseLost){
        t.Fatalf("dethroned leader was not fenced: %v",err)
    }
    if _,err=old.BeginEffect("old-run","create","crm.create",map[string]any{"id":1});!errors.Is(err,ErrExecutionLeaseLost){
        t.Fatalf("old worker dispatched after takeover: %v",err)
    }
    if _,err=next.BeginEffect("new-run","create","crm.create",map[string]any{"id":2});err!=nil{
        t.Fatalf("newly fenced leader unable to dispatch: %v",err)
    }
    // Old Close cannot remove the current leader's row because the epoch and
    // holder are part of the conditional DELETE.
    if err=old.Close();err!=nil{t.Fatal(err)}
    if err=next.CheckExecutionLease(context.Background());err!=nil {t.Fatalf("stale Close revoked new lease: %v",err)}
}

func TestSQLiteExecutionLeaseSchemaMigrationFromVersionOne(t *testing.T) {
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    // Simulate the earlier PR #20 schema. All existing revisions remain.
    db,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if _,err=db.db.Exec("DELETE FROM execution_leases");err!=nil{t.Fatal(err)}
    if _,err=db.db.Exec("DROP TABLE execution_leases");err!=nil{t.Fatal(err)}
    if _,err=db.db.Exec("PRAGMA user_version=1");err!=nil{t.Fatal(err)}
    if err=db.Close();err!=nil{t.Fatal(err)}
    upgraded,err:=New(dir,"should not overwrite stored revisions")
    if err!=nil{t.Fatal(err)}
    defer upgraded.Close()
    if err=upgraded.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    var version int
    if err=upgraded.db.QueryRow("PRAGMA user_version").Scan(&version);err!=nil{t.Fatal(err)}
    if version!=4 {t.Fatalf("schema v1 not upgraded to v4: %d",version)}
    if len(upgraded.History())!=1 {t.Fatalf("existing revision lost: %v",upgraded.History())}
}
