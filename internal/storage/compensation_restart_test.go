package storage

import (
    "errors"
    "testing"
)

func TestCompensationUnknownSurvivesRestartAndBlocksDifferentInverse(t *testing.T){
    dir:=t.TempDir()
    source:=initSource()
    first,err:=New(dir,source)
    if err!=nil{t.Fatal(err)}
    if err=first.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    originalIntent:=map[string]any{"external_id":"ABC-1"}
    inverseArgs:=map[string]any{"external_id":"ABC-1"}
    target:=TargetDigest("mcp","http://127.0.0.1:5678/mcp")
    original,err:=first.BeginEffectBound("run","create","crm.create",target,originalIntent)
    if err!=nil{t.Fatal(err)}
    if err=first.CompleteEffect(original.ID,"unknown","lost");err!=nil{t.Fatal(err)}
    if _,err=first.ResolveByOperator(original.ID,"reviewer","applied","external state checked");err!=nil{t.Fatal(err)}
    inverse,err:=first.BeginCompensation(original.ID,"crm.create",target,"crm.delete",
        originalIntent,inverseArgs,"operator","approved")
    if err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    recovered,err:=New(dir,source)
    if err!=nil{t.Fatal(err)}
    defer recovered.Close()
    if err=recovered.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    stored,err:=recovered.EffectGet(inverse.ID)
    if err!=nil || stored.Status!="unknown" || stored.Compensates!=original.ID {
        t.Fatalf("interrupted inverse was lost: %#v %v",stored,err)
    }
    _,err=recovered.BeginCompensation(original.ID,"crm.create",target,"crm.delete",
        originalIntent,inverseArgs,"operator","approved")
    if !errors.Is(err,ErrEffectAlreadyAttempted){t.Fatalf("crashed inverse retried: %v",err)}
    _,err=recovered.BeginCompensation(original.ID,"crm.create",target,"crm.other_inverse",
        originalIntent,inverseArgs,"operator","approved")
    if !errors.Is(err,ErrEffectConflict){t.Fatalf("alternate inverse replayed: %v",err)}
}
