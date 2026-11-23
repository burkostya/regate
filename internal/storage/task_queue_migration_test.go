package storage

import (
    "context"
    "errors"
    "testing"
)

func TestQueueSchemaV3UpgradePreservesTasksAndGrants(t *testing.T) {
    setQueueKeyRing(t)
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    task:=testGrantTask(t,first)
    if _,err=first.db.Exec("DROP TABLE task_queue");err!=nil{t.Fatal(err)}
    removeToolObservationSchema(t,first.db)
    if _,err=first.db.Exec("PRAGMA user_version=3");err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    upgraded,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer upgraded.Close()
    var version int
    if err=upgraded.db.QueryRow("PRAGMA user_version").Scan(&version);err!=nil{t.Fatal(err)}
    if version!=latestSQLiteVersion {t.Fatalf("schema version not upgraded to latest: %d",version)}
    existing,err:=upgraded.TaskGet(task.TaskID)
    if err!=nil || existing.OwnerDigest!=task.OwnerDigest {
        t.Fatalf("v4 migration erased persisted task: %#v %v",existing,err)
    }
    if n,err:=upgraded.QueueDepth(context.Background());err!=nil || n!=0 {
        t.Fatalf("v4 queue not created: %d %v",n,err)
    }
}
func TestQueueKeyRingRejectsMalformedAndAbsentKeys(t *testing.T) {
    for _,raw:=range []string{
        "not-json",
        `{"active":"missing","keys":{"k1":"short"}}`,
        `{"active":"k1","keys":{"k1":"aGVsbG8="}}`,
        `{"active":"bad/key","keys":{"bad/key":"aGVsbG8="}}`,
    }{
        t.Run(raw,func(t *testing.T){
            t.Setenv(taskQueueKeyEnv,raw)
            if _,err:=LoadTaskQueueKeyRing();!errors.Is(err,ErrTaskQueueKeyUnavailable){
                t.Fatalf("invalid queued-input key accepted: %v",err)
            }
        })
    }
    t.Setenv(taskQueueKeyEnv,"")
    ring,err:=LoadTaskQueueKeyRing()
    if err!=nil || ring!=nil {
        t.Fatalf("disabled queue must not require keys: %#v %v",ring,err)
    }
}
