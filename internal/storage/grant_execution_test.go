package storage

import (
    "context"
    "errors"
    "testing"
)

func grantCheckpointFixture(t *testing.T,s *Store)TaskRecord {
    t.Helper()
    task:=testGrantTask(t,s)
    task.RunID=ID()
    if err:=s.TaskPut(task);err!=nil{t.Fatal(err)}
    cp:=map[string]any{"run_id":task.RunID,"revision":task.Revision,
        "surface":task.Surface,"tool":task.Tool,"suspend_node":"wait"}
    if err:=s.PutSuspended(task.RunID,cp);err!=nil{t.Fatal(err)}
    return task
}

func TestGrantContinuationAtomicallyReservesTaskAndCredential(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=grantCheckpointFixture(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,
        "verified-refresh",expiresGrant());err!=nil{t.Fatal(err)}
    reservation,secret,err:=s.BeginTaskGrantContinuation(
        context.Background(),task.TaskID,task.OwnerDigest,32)
    if err!=nil || secret!="verified-refresh" || reservation.State!="refreshing" {
        t.Fatalf("atomic task/grant admission failed: %#v %v",reservation,err)
    }
    working,err:=s.TaskGet(task.TaskID)
    if err!=nil || working.Status!="working" ||
        working.Phase!="refreshing_grant" ||
        working.Public()["status"]!="working" {
        t.Fatalf("task not fenced during refresh: %#v %v",working,err)
    }
    if _,_,err=s.BeginTaskGrantContinuation(context.Background(),
        task.TaskID,task.OwnerDigest,32);!errors.Is(err,ErrGrantUnavailable) {
        t.Fatalf("second continuation admitted: %v",err)
    }
    if _,err=s.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest,32);err!=nil{
        t.Fatal(err)
    }
    refreshed,err:=s.FinishTaskGrantRefresh(context.Background(),reservation,
        "new-rotated-credential",expiresGrant())
    if err!=nil || refreshed.State!="active" || refreshed.Generation!=2 {
        t.Fatalf("rotated credential not durable: %#v %v",refreshed,err)
    }
    if err=s.RequireActiveTaskGrant(context.Background(),task.TaskID,
        task.OwnerDigest,refreshed.Generation);err!=nil {
        t.Fatalf("verified execution grant denied: %v",err)
    }
    if err=s.RequireActiveTaskGrant(context.Background(),task.TaskID,
        task.OwnerDigest,reservation.Generation);!errors.Is(err,ErrGrantUnavailable) {
        t.Fatalf("stale grant generation accepted: %v",err)
    }
    if err=s.RevokeTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,32);err!=nil{t.Fatal(err)}
    if err=s.RequireActiveTaskGrant(context.Background(),task.TaskID,
        task.OwnerDigest,refreshed.Generation);!errors.Is(err,ErrGrantUnavailable) {
        t.Fatalf("revoked grant was still executable: %v",err)
    }
}

func TestGrantContinuationRejectsMissingCheckpointBeforeCredentialRotation(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    task.RunID=ID()
    if err:=s.TaskPut(task);err!=nil{t.Fatal(err)}
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,
        "verified-refresh",expiresGrant());err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantContinuation(context.Background(),
        task.TaskID,task.OwnerDigest,32);!errors.Is(err,ErrGrantUnavailable) {
        t.Fatalf("missing checkpoint spent refresh credential: %v",err)
    }
    state,err:=s.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest,32)
    if err!=nil || state.State!="active" {
        t.Fatalf("missing checkpoint altered refresh state: %#v %v",state,err)
    }
}

func TestGrantContinuationCrashFailsTaskAndRefreshWithoutReplay(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=first.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    task:=grantCheckpointFixture(t,first)
    if _,err=first.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,
        "refresh-once",expiresGrant());err!=nil{t.Fatal(err)}
    grant,_,err:=first.BeginTaskGrantContinuation(context.Background(),task.TaskID,task.OwnerDigest,32)
    if err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    reopened,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer reopened.Close()
    if err=reopened.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    recovered,err:=reopened.TaskGet(task.TaskID)
    if err!=nil || recovered.Status!="failed" {
        t.Fatalf("interrupted delegated task was replayed: %#v %v",recovered,err)
    }
    state,err:=reopened.grantRowTxMust(context.Background(),task.TaskID)
    if err!=nil || state.State!="needs_reauth" || state.Generation<=grant.Generation {
        t.Fatalf("ambiguous OAuth refresh replayed: %#v %v",state,err)
    }
    if _,err=reopened.FinishTaskGrantRefresh(context.Background(),grant,
        "stale-credential",expiresGrant());!errors.Is(err,ErrGrantConflict) {
        t.Fatalf("late token result resurrected task: %v",err)
    }
}
