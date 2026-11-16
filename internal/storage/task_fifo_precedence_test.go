package storage

import (
    "context"
    "errors"
    "testing"
)

func TestFIFOAdmitsNewWorkBehindExistingBacklogEvenWithFreeSlot(t *testing.T){
    s:=queueStore(t)
    running:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),running,map[string]any{"id":"running"},1,2);err!=nil{t.Fatal(err)}
    old:=capacityTask(s,"working")
    if queued,err:=s.AdmitOrQueueTask(context.Background(),old,map[string]any{"id":"old"},1,2);
        err!=nil || !queued {t.Fatalf("old task was not queued: %v",err)}
    running.Status="completed"
    if err:=s.TaskPut(running);err!=nil{t.Fatal(err)}
    newcomer:=capacityTask(s,"working")
    queued,err:=s.AdmitOrQueueTask(context.Background(),newcomer,map[string]any{"id":"new"},1,2)
    if err!=nil || !queued {
        t.Fatalf("new task bypassed existing FIFO despite freed slot: %v",err)
    }
    first,args,err:=s.ClaimNextQueued(context.Background(),1)
    if err!=nil || first.TaskID!=old.TaskID || args["id"]!="old" {
        t.Fatalf("FIFO order was violated: %#v %#v %v",first,args,err)
    }
    first.Status="completed"
    if err=s.TaskPut(first);err!=nil{t.Fatal(err)}
    second,args,err:=s.ClaimNextQueued(context.Background(),1)
    if err!=nil || second.TaskID!=newcomer.TaskID || args["id"]!="new" {
        t.Fatalf("second queued task lost order: %#v %#v %v",second,args,err)
    }
}
func TestQueueFullStillRejectsBargingWhenActiveSlotOpens(t *testing.T){
    s:=queueStore(t)
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{},1,1);err!=nil{t.Fatal(err)}
    earlier:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),earlier,map[string]any{"id":"earlier"},1,1);err!=nil{t.Fatal(err)}
    busy.Status="completed"
    if err:=s.TaskPut(busy);err!=nil{t.Fatal(err)}
    if _,err:=s.AdmitOrQueueTask(context.Background(),capacityTask(s,"working"),
        map[string]any{"id":"barging"},1,1);!errors.Is(err,ErrTaskQueueFull) {
        t.Fatalf("overflow bypassed existing queue after slot release: %v",err)
    }
    if _,err:=s.AdmitOrQueueTask(context.Background(),capacityTask(s,"working"),
        map[string]any{"id":"barging-after-config-disable"},1,0);!errors.Is(err,ErrTaskCapacity) {
        t.Fatalf("disabled admission bypassed older queued tasks: %v",err)
    }
    if count,err:=s.ActiveTaskCount(context.Background());err!=nil || count!=0{
        t.Fatalf("barging admitted a working task: %d %v",count,err)
    }
    first,args,err:=s.ClaimNextQueued(context.Background(),1)
    if err!=nil || first.TaskID!=earlier.TaskID || args["id"]!="earlier"{
        t.Fatalf("oldest pending task not claimable: %#v %#v %v",first,args,err)
    }
}
