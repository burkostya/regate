package storage

import (
    "context"
    "errors"
    "sync"
    "testing"
    "time"
)

func capacityStore(t *testing.T)*Store {
    t.Helper()
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){_ = s.Close()})
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    return s
}
func capacityTask(s *Store,status string)TaskRecord {
    rev,_,_:=s.Active()
    return TaskRecord{
        TaskID:ID(),Status:status,OwnerDigest:"owner",
        Surface:"support",Tool:"work",Revision:rev.ID,
        CreatedAt:time.Now().UTC(),TTLMS:3600000,PollIntervalMS:1000,
    }
}
func TestAtomicCapacityAdmissionUnderParallelLoad(t *testing.T) {
    s:=capacityStore(t)
    const limit=3
    const clients=64
    var wg sync.WaitGroup
    admitted:=make(chan string,clients)
    denied:=make(chan error,clients)
    for i:=0;i<clients;i++ {
        wg.Add(1)
        go func(){
            defer wg.Done()
            task:=capacityTask(s,"working")
            err:=s.AdmitWorkingTask(context.Background(),task,limit)
            if err==nil {admitted<-task.TaskID} else {denied<-err}
        }()
    }
    wg.Wait()
    close(admitted);close(denied)
    if len(admitted)!=limit {t.Fatalf("admitted %d for limit %d",len(admitted),limit)}
    for err:=range denied {
        if !errors.Is(err,ErrTaskCapacity){t.Fatalf("unexpected admission failure: %v",err)}
    }
    n,err:=s.ActiveTaskCount(context.Background())
    if err!=nil || n!=limit {t.Fatalf("counter lost cap: %d %v",n,err)}
    // Completion frees precisely one slot.
    first:=<-admitted
    finished,err:=s.TaskGet(first)
    if err!=nil{t.Fatal(err)}
    finished.Status="completed"
    if err=s.TaskPut(finished);err!=nil{t.Fatal(err)}
    if err=s.AdmitWorkingTask(context.Background(),capacityTask(s,"working"),limit);err!=nil{
        t.Fatalf("completed slot did not release capacity: %v",err)
    }
    if n,err=s.ActiveTaskCount(context.Background());err!=nil || n!=limit {
        t.Fatalf("slot accounting after completion: %d %v",n,err)
    }
}
func TestResumeIsCapacityBoundAndCompareAndSwap(t *testing.T){
    s:=capacityStore(t)
    pending:=capacityTask(s,"input_required")
    if err:=s.TaskPut(pending);err!=nil{t.Fatal(err)}
    worker:=capacityTask(s,"working")
    if err:=s.AdmitWorkingTask(context.Background(),worker,1);err!=nil{t.Fatal(err)}
    resumed:=pending
    resumed.Status="working"
    if err:=s.ClaimTaskContinuation(context.Background(),resumed,1);
        !errors.Is(err,ErrTaskCapacity) {t.Fatalf("resume bypassed capacity: %v",err)}
    still,err:=s.TaskGet(pending.TaskID)
    if err!=nil || still.Status!="input_required" {
        t.Fatalf("capacity refusal mutated pending task: %#v %v",still,err)
    }
    worker.Status="completed"
    if err=s.TaskPut(worker);err!=nil{t.Fatal(err)}
    if err=s.ClaimTaskContinuation(context.Background(),resumed,1);err!=nil {
        t.Fatalf("authorized continuation rejected: %v",err)
    }
    if err=s.ClaimTaskContinuation(context.Background(),resumed,1);
        !errors.Is(err,ErrTaskTransition) {
        t.Fatalf("same checkpoint admitted twice: %v",err)
    }
}
func TestCapacityAdmissionRequiresExecutionLeader(t *testing.T){
    leader:=capacityStore(t)
    observer,err:=New(leader.Dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer observer.Close()
    if err=observer.AdmitWorkingTask(context.Background(),capacityTask(observer,"working"),1);
        !errors.Is(err,ErrExecutionLeaseHeld) {
        t.Fatalf("observer admitted a task: %v",err)
    }
}
func TestGrantBoundContinuationDoesNotOversubscribe(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=capacityStore(t)
    pending:=grantCheckpointFixture(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),pending.TaskID,pending.OwnerDigest,
        "sealed-refresh",expiresGrant());err!=nil{t.Fatal(err)}
    busy:=capacityTask(s,"working")
    if err:=s.AdmitWorkingTask(context.Background(),busy,1);err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantContinuation(context.Background(),
        pending.TaskID,pending.OwnerDigest,1);!errors.Is(err,ErrTaskCapacity){
        t.Fatalf("grant-backed resume bypassed active slot: %v",err)
    }
    state,err:=s.TaskGrantState(context.Background(),pending.TaskID,pending.OwnerDigest)
    if err!=nil || state.State!="active" {
        t.Fatalf("capacity refusal spent encrypted refresh token: %#v %v",state,err)
    }
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    approved,token,err:=s.BeginTaskGrantContinuation(context.Background(),
        pending.TaskID,pending.OwnerDigest,1)
    if err!=nil || approved.State!="refreshing" || token!="sealed-refresh" {
        t.Fatalf("grant continuation could not acquire released slot: %#v %v",approved,err)
    }
    if n,err=s.ActiveTaskCount(context.Background());err!=nil || n!=1 {
        t.Fatalf("grant continuation double counted: %d %v",n,err)
    }
}
func TestInvalidLimitsRefusedAtStoreBoundary(t *testing.T){
    s:=capacityStore(t)
    for _,limit:=range []int{-1,0,1025}{
        if err:=s.AdmitWorkingTask(context.Background(),capacityTask(s,"working"),limit);err==nil {
            t.Fatalf("invalid limit admitted: %d",limit)
        }
    }
}
