package config

import (
    "strings"
    "testing"
)

const validReadbackHCL = `
readback "crm_check" {
  write_operation = "crm.create"
  upstream = "crm"
  read_tool = "find"
  intent_pointer = "/external_id"
  read_argument = "external_id"
  result_pointer = "/structuredContent/record/external_id"
  safe_read = true
}
`

func TestReadbackHCLContract(t *testing.T) {
    cfg,err:=Decode(example(t)+"\n"+validReadbackHCL)
    if err!=nil {t.Fatal(err)}
    read,ok:=cfg.Readbacks["crm_check"]
    if !ok || !read.SafeRead || read.WriteOperation!="crm.create" ||
        read.IntentPointer!="/external_id" || read.ResultPointer!="/structuredContent/record/external_id" {
        t.Fatalf("readback config corrupted: %#v",read)
    }
}

func TestReadbackRejectsUnsafeOrUnboundDeclarations(t *testing.T) {
    for _,tc:=range []struct{name,old,new string}{
        {"missing-read-assertion","safe_read = true","safe_read = false"},
        {"unknown-upstream",`upstream = "crm"`,`upstream = "not-an-upstream"`},
        {"wrong-write-target",`write_operation = "crm.create"`,`write_operation = "agent.create"`},
        {"missing-write-target",`write_operation = "crm.create"`,`write_operation = ""`},
        {"missing-pointer",`intent_pointer = "/external_id"`,`intent_pointer = ""`},
        {"bad-pointer-escape",`result_pointer = "/structuredContent/record/external_id"`,`result_pointer = "/record/~2invalid"`},
        {"invalid-read-argument",`read_argument = "external_id"`,`read_argument = "external id"`},
        {"unknown-field",`safe_read = true`,`safe_read = true
  trust_all = true`},
    }{
        t.Run(tc.name,func(t *testing.T) {
            mutated:=strings.Replace(validReadbackHCL,tc.old,tc.new,1)
            if _,err:=Decode(example(t)+"\n"+mutated);err==nil {
                t.Fatalf("accepted unsafe readback: %s",mutated)
            }
        })
    }
}
