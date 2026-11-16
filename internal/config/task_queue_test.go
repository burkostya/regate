package config

import (
    "strings"
    "testing"
)

func TestBoundedWaitingQueueConfig(t *testing.T){
    base:=example(t)
    original,err:=Decode(base)
    if err!=nil{t.Fatal(err)}
    if original.MaxQueuedTasks!=0 || original.MaxQueuedPerOwner!=32 {
        t.Fatalf("wrong default queue quotas: total=%d owner=%d",
            original.MaxQueuedTasks,original.MaxQueuedPerOwner)
    }
    for _,test:=range []struct{expression string;valid bool;expected int}{
        {"max_queued_tasks = 0",true,0},
        {"max_queued_tasks = 7",true,7},
        {"max_queued_tasks = 4096",true,4096},
        {"max_queued_tasks = -1",false,0},
        {"max_queued_tasks = 4097",false,0},
        {"max_queued_tasks = 3.5",false,0},
        {`max_queued_tasks = "7"`,false,0},
    } {
        src:=strings.Replace(base,`listen = "127.0.0.1:8080"`,
            `listen = "127.0.0.1:8080"
  `+test.expression,1)
        cfg,err:=Decode(src)
        if test.valid {
            if err!=nil || cfg.MaxQueuedTasks!=test.expected {
                t.Fatalf("valid queue config %q rejected: %#v %v",test.expression,cfg,err)
            }
        } else if err==nil {
            t.Fatalf("unsafe queue config accepted: %s",test.expression)
        }
    }
}

func TestPerOwnerWaitingQuotaBounds(t *testing.T) {
    base:=example(t)
    for _,test:=range []struct{expression string;valid bool;expected int}{
        {"max_queued_per_owner = 1",true,1},
        {"max_queued_per_owner = 17",true,17},
        {"max_queued_per_owner = 4096",true,4096},
        {"max_queued_per_owner = 0",false,0},
        {"max_queued_per_owner = -1",false,0},
        {"max_queued_per_owner = 4097",false,0},
        {"max_queued_per_owner = 2.5",false,0},
        {`max_queued_per_owner = "3"`,false,0},
    } {
        src:=strings.Replace(base,`listen = "127.0.0.1:8080"`,
            `listen = "127.0.0.1:8080"
  `+test.expression,1)
        cfg,err:=Decode(src)
        if test.valid {
            if err!=nil || cfg.MaxQueuedPerOwner!=test.expected {
                t.Fatalf("valid owner quota %s rejected: %#v %v",test.expression,cfg,err)
            }
        }else if err==nil {
            t.Fatalf("unsafe owner quota accepted: %s",test.expression)
        }
    }
}
