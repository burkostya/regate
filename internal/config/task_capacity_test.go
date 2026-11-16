package config

import (
    "strings"
    "testing"
)

func TestMaxActiveTasksHCLDefaultAndBounds(t *testing.T) {
    original,err:=Decode(example(t))
    if err!=nil{t.Fatal(err)}
    if original.MaxActiveTasks!=32 {
        t.Fatalf("unexpected default async capacity: %d",original.MaxActiveTasks)
    }
    allowed:=strings.Replace(example(t),`listen = "127.0.0.1:8080"`,
        `listen = "127.0.0.1:8080"
  max_active_tasks = 3`,1)
    cfg,err:=Decode(allowed)
    if err!=nil || cfg.MaxActiveTasks!=3 {
        t.Fatalf("custom task bound rejected: %#v %v",cfg,err)
    }
    for _,bad:=range []string{
        `max_active_tasks = 0`,
        `max_active_tasks = -1`,
        `max_active_tasks = 1025`,
        `max_active_tasks = "ten"`,
        `max_active_tasks = 3.5`,
    } {
        changed:=strings.Replace(example(t),`listen = "127.0.0.1:8080"`,
            `listen = "127.0.0.1:8080"
  `+bad,1)
        if _,err=Decode(changed);err==nil {
            t.Fatalf("unsafe active task limit accepted: %s",bad)
        }
    }
}
