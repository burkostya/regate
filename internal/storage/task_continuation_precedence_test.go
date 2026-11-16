package storage

import (
    "context"
    "errors"
    "testing"
)

// Continuations and new queue-eligible task admissions must obey one
// precedence rule: already accepted, encrypted waiting work is never
// displaced when a working slot is briefly free.
func TestInteractiveResumeCannotBypassOlderQueuedWork(t *testing.T){
    s:=queueStore(t)
    ctx:=context.Background()
    pending:=capacityTask(s,"input_required")
    if err:=s.TaskPut(pending);err!=nil{t.Fatal(err)}
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{"id":"busy"},1,1);err!=nil{t.Fatal(err)}
    earlier:=capacityTask(s,"working")
    queued,err:=s.AdmitOrQueueTask(ctx,earlier,map[string]any{"id":"old"},1,1)
    if err!=nil || !queued {t.Fatalf("earlier job was not queued: %v",err)}
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}

    requested:=pending
    requested.Status="working"
    if err=s.ClaimTaskContinuation(ctx,requested,1);!errors.Is(err,ErrTaskBacklog) {
        t.Fatalf("resume cut ahead of existing backlog: %v",err)
    }
    stored,err:=s.TaskGet(pending.TaskID)
    if err!=nil || stored.Status!="input_required" {
        t.Fatalf("rejected resume mutated task: %#v %v",stored,err)
    }
    first,args,err:=s.ClaimNextQueued(ctx,1)
    if err!=nil || first.TaskID!=earlier.TaskID || args["id"]!="old" {
        t.Fatalf("older job was skipped: %#v %#v %v",first,args,err)
    }
    first.Status="completed"
    if err=s.TaskPut(first);err!=nil{t.Fatal(err)}
    if err=s.ClaimTaskContinuation(ctx,requested,1);err!=nil {
        t.Fatalf("resume rejected after backlog cleared: %v",err)
    }
}

func TestGrantBackedResumeCannotConsumeTokenAheadOfBacklog(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=queueStore(t)
    ctx:=context.Background()
    awaiting:=grantCheckpointFixture(t,s)
    if _,err:=s.CreateTaskGrant(ctx,awaiting.TaskID,awaiting.OwnerDigest,
        "verified-refresh",expiresGrant());err!=nil{t.Fatal(err)}

    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(ctx,busy,map[string]any{},1,1);err!=nil{t.Fatal(err)}
    earlier:=capacityTask(s,"working")
    queued,err:=s.AdmitOrQueueTask(ctx,earlier,map[string]any{"id":"old"},1,1)
    if err!=nil || !queued{t.Fatalf("could not queue earlier task: %v",err)}
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}

    if _,_,err=s.BeginTaskGrantContinuation(ctx,awaiting.TaskID,awaiting.OwnerDigest,1);
        !errors.Is(err,ErrTaskBacklog) {
        t.Fatalf("grant-backed resume bypassed queue: %v",err)
    }
    grant,err:=s.TaskGrantState(ctx,awaiting.TaskID,awaiting.OwnerDigest)
    if err!=nil || grant.State!="active" || grant.Generation!=1 {
        t.Fatalf("backlog rejection consumed refresh credential: %#v %v",grant,err)
    }
    task,err:=s.TaskGet(awaiting.TaskID)
    if err!=nil || task.Status!="input_required" {
        t.Fatalf("backlog rejection mutated delegated task: %#v %v",task,err)
    }
    served,args,err:=s.ClaimNextQueued(ctx,1)
    if err!=nil || served.TaskID!=earlier.TaskID || args["id"]!="old" {
        t.Fatalf("queue head was not served first: %#v %#v %v",served,args,err)
    }
    served.Status="completed"
    if err=s.TaskPut(served);err!=nil{t.Fatal(err)}
    reserved,secret,err:=s.BeginTaskGrantContinuation(
        ctx,awaiting.TaskID,awaiting.OwnerDigest,1)
    if err!=nil || reserved.State!="refreshing" || secret!="verified-refresh" {
        t.Fatalf("resumed after backlog did not reserve verified token: %#v %v",reserved,err)
    }
}
