package storage

import (
    "errors"
    "fmt"
    "strings"
    "sync"
    "testing"
)

func compensationFixture(t *testing.T)(*Store,Effect,map[string]any,string) {
    t.Helper()
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){_ = s.Close()})
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    originalArgs:=map[string]any{"external_id":"C123"}
    endpoint:=TargetDigest("mcp","http://127.0.0.1:9999/mcp")
    original,err:=s.BeginEffectBound("run-create","create","crm.create",endpoint,originalArgs)
    if err!=nil{t.Fatal(err)}
    if err=s.CompleteEffect(original.ID,"unknown","lost response");err!=nil{t.Fatal(err)}
    resolved,err:=s.ResolveByOperator(original.ID,"reviewer-1","applied","verified creation on CRM")
    if err!=nil{t.Fatal(err)}
    return s,resolved,originalArgs,endpoint
}

func TestCompensationAuthorizesOnceAndPreservesSource(t *testing.T){
    s,source,origArgs,target:=compensationFixture(t)
    args:=map[string]any{"external_id":"C123"}
    secretReason:="approved via ticket containing alice@example.com and private key"
    comp,err:=s.BeginCompensation(source.ID,"crm.create",target,"crm.delete",
        origArgs,args,"operator-42",secretReason)
    if err!=nil{t.Fatal(err)}
    if comp.Status!="dispatching" || comp.Compensates!=source.ID ||
        comp.ID!=CompensationID(source.ID) || comp.Approval==nil ||
        comp.Approval.Actor!="operator-42" || len(comp.Approval.ReasonDigest)!=64 {
        t.Fatalf("missing durable compensation identity/audit: %#v",comp)
    }
    if strings.Contains(fmt.Sprintf("%#v",comp),secretReason) ||
        strings.Contains(fmt.Sprintf("%#v",comp),"alice@example.com") {
        t.Fatal("raw operator explanation persisted")
    }
    saved,err:=s.EffectGet(source.ID)
    if err!=nil || saved.Status!="resolved_applied" || saved.Approval!=nil {
        t.Fatalf("original source effect mutated: %#v %v",saved,err)
    }
    if err=s.CompleteEffect(comp.ID,"unknown","connection reset");err!=nil{t.Fatal(err)}
    if _,err=s.BeginCompensation(source.ID,"crm.create",target,"crm.delete",
        origArgs,args,"operator-42",secretReason);!errors.Is(err,ErrEffectAlreadyAttempted){
        t.Fatalf("unknown compensation replay admitted: %v",err)
    }
    if _,err=s.BeginCompensation(source.ID,"crm.create",target,"crm.other",
        origArgs,args,"operator-42",secretReason);!errors.Is(err,ErrEffectConflict){
        t.Fatalf("alternate compensation tool repeated effect: %v",err)
    }
    if _,err=s.BeginCompensation(source.ID,"crm.create",target,"crm.delete",
        origArgs,map[string]any{"external_id":"C999"},"operator-42",secretReason);!errors.Is(err,ErrEffectConflict){
        t.Fatalf("alternate inverse parameters reused logical attempt: %v",err)
    }
}

func TestCompensationRequiresResolvedAppliedAndVerifiedOriginal(t *testing.T){
    s,source,args,target:=compensationFixture(t)
    correct:=map[string]any{"external_id":"C123"}
    cases:=[]struct{name string;original string;target string;oldArgs any;actor string;reason string}{
        {"wrong-write", "crm.update",target,args,"op","approved"},
        {"wrong-target", "crm.create",TargetDigest("mcp","http://127.0.0.1:8888"),args,"op","approved"},
        {"wrong-args", "crm.create",target,map[string]any{"external_id":"OTHER"},"op","approved"},
        {"missing-actor","crm.create",target,args,"","approved"},
        {"missing-reason","crm.create",target,args,"op",""},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T){
            if _,err:=s.BeginCompensation(source.ID,tc.original,tc.target,"crm.delete",
                tc.oldArgs,correct,tc.actor,tc.reason);!errors.Is(err,ErrInvalidEvidence){
                t.Fatalf("unsafe compensation accepted: %v",err)
            }
        })
    }
    unproven,err:=s.BeginEffectBound("unproven","create","crm.create",target,args)
    if err!=nil{t.Fatal(err)}
    if _,err=s.BeginCompensation(unproven.ID,"crm.create",target,"crm.delete",
        args,correct,"op","approved");!errors.Is(err,ErrInvalidEvidence){
        t.Fatalf("dispatching source compensated: %v",err)
    }
    if err=s.CompleteEffect(unproven.ID,"confirmed","");err!=nil{t.Fatal(err)}
    if _,err=s.BeginCompensation(unproven.ID,"crm.create",target,"crm.delete",
        args,correct,"op","approved");!errors.Is(err,ErrInvalidEvidence){
        t.Fatalf("mere transport confirmation allowed inverse: %v",err)
    }
    if _,err=s.BeginEffectBound("unresolved","create","crm.create",target,args);err!=nil{t.Fatal(err)}
}

func TestCompensationConcurrentAdmissionSingleWinner(t *testing.T){
    s,source,args,target:=compensationFixture(t)
    const workers=24
    var wg sync.WaitGroup
    successes:=make(chan string,workers)
    errorsOut:=make(chan error,workers)
    for i:=0;i<workers;i++ {
        wg.Add(1)
        go func(){
            defer wg.Done()
            e,err:=s.BeginCompensation(source.ID,"crm.create",target,"crm.delete",
                args,map[string]any{"external_id":"C123"},"operator-42","reviewed")
            if err!=nil {errorsOut<-err;return}
            successes<-e.ID
        }()
    }
    wg.Wait()
    close(successes);close(errorsOut)
    if len(successes)!=1 {t.Fatalf("expected one admitted compensation, got %d",len(successes))}
    for err:=range errorsOut {
        if !errors.Is(err,ErrEffectAlreadyAttempted){
            t.Fatalf("unexpected compensation concurrency error: %v",err)
        }
    }
}
