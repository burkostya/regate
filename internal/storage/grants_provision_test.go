package storage

import (
    "context"
    "errors"
    "sync"
    "testing"
)

func TestInitialGrantProvisionReservationIsOneShot(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    reserved,err:=s.ReserveTaskGrantProvision(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil{t.Fatal(err)}
    if reserved.State!="refreshing" || reserved.Generation!=1 {
        t.Fatalf("missing durable reservation: %#v",reserved)
    }
    if _,err=s.ReserveTaskGrantProvision(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantConflict) {
        t.Fatalf("provider rotation allowed twice: %v",err)
    }
    row,err:=s.grantRowTxMust(context.Background(),task.TaskID)
    if err!=nil{t.Fatal(err)}
    if len(row.nonce)!=0 || len(row.ciphertext)!=0 {
        t.Fatal("unverified credential was admitted")
    }
    // Only after a higher-level trusted OAuth verifier returned a new
    // credential may the reserved grant become available for future use.
    confirmed,err:=s.FinishTaskGrantRefresh(context.Background(),
        reserved,"idp-verified-rotated-refresh",expiresGrant())
    if err!=nil || confirmed.State!="active" || confirmed.Generation!=2 {
        t.Fatalf("verified result was not committed: %#v %v",confirmed,err)
    }
    next,secret,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || secret!="idp-verified-rotated-refresh" ||
        next.Generation!=confirmed.Generation {
        t.Fatalf("sealed rotating credential cannot be opened: %#v %v",next,err)
    }
}
func (s *Store) grantRowTxMust(ctx context.Context,taskID string)(taskGrant,error) {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return taskGrant{},err}
    defer tx.Rollback()
    return s.grantRowTx(tx,taskID)
}

func TestInitialGrantProvisionCrashBecomesNeedsReauth(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    dir:=t.TempDir()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    task:=testGrantTask(t,s)
    reserved,err:=s.ReserveTaskGrantProvision(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil{t.Fatal(err)}
    if err=s.Close();err!=nil{t.Fatal(err)}
    recovered,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer recovered.Close()
    if err=recovered.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    state,err:=recovered.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || state.State!="needs_reauth" || state.Generation<=reserved.Generation {
        t.Fatalf("lost provider rotation was retried: %#v %v",state,err)
    }
    if _,err=recovered.ReserveTaskGrantProvision(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantConflict) {t.Fatalf("crashed refresh was repeated: %v",err)}
    if _,err=recovered.FinishTaskGrantRefresh(context.Background(),reserved,
        "credential-after-takeover",expiresGrant());!errors.Is(err,ErrGrantConflict) {
        t.Fatalf("late refresh resurrected recovered grant: %v",err)
    }
}

func TestInitialGrantProvisionConcurrentOnlyOneIdpAttempt(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    var wg sync.WaitGroup
    wins:=make(chan GrantMetadata,16)
    errs:=make(chan error,16)
    for i:=0;i<16;i++ {
        wg.Add(1)
        go func(){
            defer wg.Done()
            g,err:=s.ReserveTaskGrantProvision(context.Background(),task.TaskID,task.OwnerDigest)
            if err!=nil{errs<-err}else{wins<-g}
        }()
    }
    wg.Wait()
    close(wins);close(errs)
    if len(wins)!=1{t.Fatalf("concurrent initial grant admitted %d times",len(wins))}
    for err:=range errs {
        if !errors.Is(err,ErrGrantConflict) {
            t.Fatalf("unexpected concurrent admission outcome: %v",err)
        }
    }
}
