package storage

import (
    "context"
    "errors"
    "sync"
    "testing"
)

func fairQueueTask(s *Store,owner string)TaskRecord {
    task:=capacityTask(s,"working")
    task.OwnerDigest=owner
    return task
}

func TestFairQueueServesOwnersRoundRobinAndPreservesOwnerFIFO(t *testing.T) {
    s:=queueStore(t)
    ctx:=context.Background()
    active:=fairQueueTask(s,"owner-A")
    if queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,active,
        map[string]any{"label":"active"},1,10,4);err!=nil || queued {
        t.Fatalf("active admission failed: %v",err)
    }
    type example struct {owner,label string;task TaskRecord}
    backlog:=[]example{
        {owner:"owner-A",label:"A1"},
        {owner:"owner-A",label:"A2"},
        {owner:"owner-A",label:"A3"},
        {owner:"owner-B",label:"B1"},
        {owner:"owner-B",label:"B2"},
        {owner:"owner-C",label:"C1"},
    }
    for i:=range backlog{
        item:=&backlog[i]
        item.task=fairQueueTask(s,item.owner)
        queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,item.task,
            map[string]any{"label":item.label},1,10,4)
        if err!=nil || !queued {t.Fatalf("failed to queue %s: %v",item.label,err)}
    }
    active.Status="completed"
    if err:=s.TaskPut(active);err!=nil{t.Fatal(err)}

    expected:=[]string{"A1","B1","C1","A2","B2","A3"}
    for _,want:=range expected {
        task,args,err:=s.ClaimNextQueued(ctx,1)
        if err!=nil{t.Fatal(err)}
        if got:=args["label"];got!=want {
            t.Fatalf("owner round-robin order wrong: got %v want %s",got,want)
        }
        if task.Status!="working" {t.Fatalf("wrong claim state: %#v",task)}
        task.Status="completed"
        if err:=s.TaskPut(task);err!=nil{t.Fatal(err)}
    }
    if depth,err:=s.QueueDepth(ctx);err!=nil || depth!=0 {
        t.Fatalf("queue not drained: %d %v",depth,err)
    }
    var activeTurns int
    if err:=s.db.QueryRow("SELECT COUNT(*) FROM task_queue_turns").Scan(&activeTurns);
        err!=nil || activeTurns!=0{
        t.Fatalf("orphan scheduling state was not cleaned: %d %v",activeTurns,err)
    }
}

func TestFairOwnerTurnsPersistAcrossRestart(t *testing.T) {
    setQueueKeyRing(t)
    dir:=t.TempDir()
    ctx:=context.Background()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    busy:=fairQueueTask(s,"owner-A")
    if _,err=s.AdmitOrQueueTaskWithOwnerLimit(ctx,busy,map[string]any{},1,6,4);err!=nil{t.Fatal(err)}
    for _,pair:=range []struct{owner,label string}{
        {"owner-A","A1"},{"owner-A","A2"},{"owner-B","B1"},{"owner-B","B2"},
    } {
        task:=fairQueueTask(s,pair.owner)
        queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,task,
            map[string]any{"label":pair.label},1,6,4)
        if err!=nil || !queued{t.Fatalf("queue %s: %v",pair.label,err)}
    }
    busy.Status="completed"
    if err=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    served,args,err:=s.ClaimNextQueued(ctx,1)
    if err!=nil || args["label"]!="A1" {t.Fatalf("initial scheduling: %v %#v",err,args)}
    served.Status="completed"
    if err=s.TaskPut(served);err!=nil{t.Fatal(err)}
    if err=s.Close();err!=nil{t.Fatal(err)}

    restarted,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer restarted.Close()
    if err=restarted.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    second,args,err:=restarted.ClaimNextQueued(ctx,1)
    if err!=nil || args["label"]!="B1" {
        t.Fatalf("restart erased fairness turn; got %#v error %v",args,err)
    }
    second.Status="completed"
    if err=restarted.TaskPut(second);err!=nil{t.Fatal(err)}
    third,args,err:=restarted.ClaimNextQueued(ctx,1)
    if err!=nil || args["label"]!="A2" {
        t.Fatalf("owner turn did not rotate: %#v %v",args,err)
    }
    third.Status="completed"
    if err=restarted.TaskPut(third);err!=nil{t.Fatal(err)}
    fourth,args,err:=restarted.ClaimNextQueued(ctx,1)
    if err!=nil || args["label"]!="B2" {
        t.Fatalf("owner FIFO was lost after restart: %#v %v",args,err)
    }
}

func TestBoundedPerOwnerQueueQuotaAndIndependentOwners(t *testing.T) {
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=fairQueueTask(s,"owner-A")
    if _,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,busy,map[string]any{},1,8,2);
        err!=nil {t.Fatal(err)}
    for i:=0;i<2;i++{
        task:=fairQueueTask(s,"owner-A")
        if queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,task,
            map[string]any{"id":i},1,8,2);err!=nil || !queued{
            t.Fatalf("owner quota refused %d prematurely: %v",i,err)
        }
    }
    if _,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,
        fairQueueTask(s,"owner-A"),map[string]any{"id":"third"},1,8,2);
        !errors.Is(err,ErrTaskOwnerQueueFull){
        t.Fatalf("owner-A quota did not prevent flood: %v",err)
    }
    if queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,
        fairQueueTask(s,"owner-B"),map[string]any{"id":"B1"},1,8,2);
        err!=nil || !queued {
        t.Fatalf("owner-B was starved by owner-A quota: %v",err)
    }
    if count,err:=s.QueueDepth(ctx);err!=nil || count!=3 {
        t.Fatalf("quota rejection inserted queued task: %d %v",count,err)
    }
}

func TestConcurrentSameOwnerCannotOverrunQuota(t *testing.T){
    s:=queueStore(t)
    ctx:=context.Background()
    busy:=fairQueueTask(s,"owner-A")
    if _,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,busy,map[string]any{},1,16,3);
        err!=nil{t.Fatal(err)}
    var wg sync.WaitGroup
    accepted:=make(chan struct{},32)
    denied:=make(chan error,32)
    for i:=0;i<32;i++{
        wg.Add(1)
        go func(i int){
            defer wg.Done()
            task:=fairQueueTask(s,"owner-A")
            queued,err:=s.AdmitOrQueueTaskWithOwnerLimit(ctx,task,
                map[string]any{"i":i},1,16,3)
            if err!=nil {denied<-err;return}
            if !queued {denied<-errors.New("task bypassed queue");return}
            accepted<-struct{}{}
        }(i)
    }
    wg.Wait()
    close(accepted);close(denied)
    if len(accepted)!=3 {t.Fatalf("accepted %d tasks against quota 3",len(accepted))}
    for err:=range denied {
        if !errors.Is(err,ErrTaskOwnerQueueFull) {
            t.Fatalf("incorrect concurrent quota error: %v",err)
        }
    }
}
