package storage

import (
    "strings"
    "sync"
    "testing"
)

func TestAuditChainRestartParallelAndTamperRefusal(t *testing.T) {
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil {t.Fatal(err)}
    second,err:=New(dir,initSource())
    if err!=nil {t.Fatal(err)}
    var wg sync.WaitGroup
    errors:=make(chan error,32)
    for i:=0;i<32;i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            store:=first
            if i%2==1 {store=second}
            errors<-store.AppendAudit("management","regate_activate","started")
        }(i)
    }
    wg.Wait()
    close(errors)
    for err:=range errors {if err!=nil {t.Fatalf("concurrent audit failed: %v",err)}}
    audit,err:=first.AuditTail(200)
    if err!=nil || len(audit)!=32 {t.Fatalf("lost audit events: %d %v",len(audit),err)}
    if audit[0].Sequence!=32 || audit[31].Sequence!=1 {t.Fatalf("audit ordering failed: %#v",audit)}
    if err=first.Close();err!=nil {t.Fatal(err)}
    if err=second.Close();err!=nil {t.Fatal(err)}
    reopened,err:=New(dir,"ignored")
    if err!=nil {t.Fatalf("audit validation on restart: %v",err)}
    if err=reopened.AuditVerify();err!=nil {t.Fatal(err)}
    if _,err=reopened.db.Exec("UPDATE audit_events SET outcome='denied' WHERE seq=3");err!=nil {t.Fatal(err)}
    if _,err=reopened.AuditTail(200);err==nil || !strings.Contains(err.Error(),"digest verification") {
        t.Fatalf("tampered audit went undetected: %v",err)
    }
    if err=reopened.AppendAudit("management","regate_activate","started");err==nil {
        t.Fatal("mutable administrative journal proceeded after audit corruption")
    }
    if err=reopened.Close();err!=nil {t.Fatal(err)}
    if _,err=New(dir,"ignored");err==nil {t.Fatal("startup accepted corrupt audit trail")}
}

func TestAuditRejectedInputsNeverPersist(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil {t.Fatal(err)}
    defer s.Close()
    for _,e:=range []struct{category,action,outcome string}{
        {"management","regate_activate\npassword=secret","started"},
        {"management","customer:alice@example.com","started"},
        {"unknown","regate_activate","started"},
        {"management","regate_activate","password=secret"},
    } {
        if err=s.AppendAudit(e.category,e.action,e.outcome);err==nil {t.Fatalf("accepted unsafe audit event %#v",e)}
    }
    got,err:=s.AuditTail(200)
    if err!=nil || len(got)!=0 {t.Fatalf("unsafe audit events persisted: %#v %v",got,err)}
}
