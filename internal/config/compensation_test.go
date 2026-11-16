package config

import (
    "strings"
    "testing"
)

const validCompensationHCL = `
compensation "undo_create" {
  for_operation = "crm.create"
  upstream = "crm"
  tool = "delete_customer"
  intent_pointer = "/external_id"
  argument = "external_id"
  enabled = true
}
`

func TestCompensationHCLContract(t *testing.T) {
    cfg,err:=Decode(example(t)+"\n"+validCompensationHCL)
    if err!=nil{t.Fatal(err)}
    got,ok:=cfg.Compensations["undo_create"]
    if !ok || !got.Enabled || got.ForOperation!="crm.create" ||
        got.Tool!="delete_customer" || got.IntentPointer!="/external_id" {
        t.Fatalf("compensation config corrupt: %#v",got)
    }
}

func TestRejectUnsafeCompensationHCL(t *testing.T) {
    cases:=[]struct{name,old,new string}{
        {"disabled",`enabled = true`,`enabled = false`},
        {"wrong-upstream",`upstream = "crm"`,`upstream = "missing"`},
        {"wrong-source",`for_operation = "crm.create"`,`for_operation = "agent.create"`},
        {"missing-tool",`tool = "delete_customer"`,`tool = ""`},
        {"empty-pointer",`intent_pointer = "/external_id"`,`intent_pointer = ""`},
        {"bad-pointer",`intent_pointer = "/external_id"`,`intent_pointer = "/bad~2escape"`},
        {"bad-argument",`argument = "external_id"`,`argument = "external id"`},
        {"unknown-field",`enabled = true`,`enabled = true
  allow_automatic = true`},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T){
            src:=strings.Replace(validCompensationHCL,tc.old,tc.new,1)
            if _,err:=Decode(example(t)+"\n"+src);err==nil {
                t.Fatalf("unsafe inverse contract accepted: %s",src)
            }
        })
    }
}
