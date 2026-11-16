package server

import (
    "context"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/storage"
)

func TestDispatcherPurgesExpiredQueueWhileActiveWorkerIsStillBlocked(t *testing.T){
    queuedTestKey(t)
    backend,calls,unblock:=queueServerFixture(t)
    defer backend.Close()
    defer unblock()

    svc,err:=New(t.TempDir(),queueConfig(backend.URL))
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()

    active:=startQueueTask(t,svc,"active")
    awaitQueueEvent(t,calls,"active")

    _,revision:=svc.Current()
    old:=storage.TaskRecord{
        TaskID:storage.ID(),Status:"working",OwnerDigest:"owner",
        Surface:"work",Tool:"wait",Revision:revision.ID,
        CreatedAt:time.Now().Add(-time.Hour),TTLMS:10,
        PollIntervalMS:1000,
    }
    queued,err:=svc.Store.AdmitOrQueueTask(context.Background(),old,
        map[string]any{"id":"expired-private-input"},1,2)
    if err!=nil || !queued {t.Fatalf("expired queued fixture rejected: %v",err)}

    // The active upstream remains deliberately blocked. The dispatcher
    // must expire the queued ciphertext independently of available slots.
    deadline:=time.Now().Add(5*time.Second)
    removed:=false
    for time.Now().Before(deadline){
        n,err:=svc.Store.QueueDepth(context.Background())
        if err!=nil{t.Fatal(err)}
        if n==0 {removed=true;break}
        time.Sleep(25*time.Millisecond)
    }
    if !removed {t.Fatal("expired queue remained full while active slot busy")}
    if activeCount,err:=svc.Store.ActiveTaskCount(context.Background());
        err!=nil || activeCount!=1{
        t.Fatalf("expiry dispatched or removed active worker: %d %v",activeCount,err)
    }
    fresh:=startQueueTask(t,svc,"fresh")
    if depth,err:=svc.Store.QueueDepth(context.Background());err!=nil || depth!=1{
        t.Fatalf("new task could not use expired queue capacity: %d %v",depth,err)
    }

    unblock()
    awaitQueueEvent(t,calls,"fresh")
    awaitQueuedTask(t,svc.Store,active,"completed")
    awaitQueuedTask(t,svc.Store,fresh,"completed")
    select {
    case extra:=<-calls:
        t.Fatalf("expired task executed despite TTL: %s",extra)
    default:
    }
}
