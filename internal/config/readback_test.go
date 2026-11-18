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

func TestReadbackOptionalSemanticMatchContract(t *testing.T) {
    attr := "  match_intent_pointer = \"/value\"\n  match_result_pointer = \"/structuredContent/record/value\"\n"
    valid := strings.Replace(validReadbackHCL, "  safe_read = true\n", attr+"  safe_read = true\n", 1)
    cfg,err:=Decode(example(t)+"\n"+valid)
    if err!=nil {t.Fatalf("valid semantic match rejected: %v",err)}
    got:=cfg.Readbacks["crm_check"]
    if got.MatchIntentPointer!="/value" || got.MatchResultPointer!="/structuredContent/record/value" {
        t.Fatalf("semantic match was not compiled: %#v",got)
    }
    cases:=[]struct{name,source string}{
        {"missing-result-pointer",strings.Replace(valid, "  match_result_pointer = \"/structuredContent/record/value\"\n", "",1)},
        {"missing-intent-pointer",strings.Replace(valid, "  match_intent_pointer = \"/value\"\n", "",1)},
        {"invalid-intent-escape",strings.Replace(valid, "match_intent_pointer = \"/value\"", "match_intent_pointer = \"/bad~3pointer\"",1)},
        {"invalid-result-escape",strings.Replace(valid, "match_result_pointer = \"/structuredContent/record/value\"", "match_result_pointer = \"/bad~2pointer\"",1)},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T) {
            if _,err:=Decode(example(t)+"\n"+tc.source);err==nil {
                t.Fatalf("unsafe readback pair accepted: %s",tc.source)
            }
        })
    }
}

func TestReadbackPerEffectWriteTokenContract(t *testing.T) {
    pair := "  write_token_argument = \"operation_token\"\n  result_token_pointer = \"/structuredContent/record/operation_token\"\n"
    valid := strings.Replace(validReadbackHCL,"  safe_read = true\n",pair+"  safe_read = true\n",1)
    cfg,err:=Decode(example(t)+"\n"+valid)
    if err!=nil {t.Fatalf("valid token contract rejected: %v",err)}
    rule:=cfg.Readbacks["crm_check"]
    if rule.WriteTokenArgument!="operation_token" ||
        rule.ResultTokenPointer!="/structuredContent/record/operation_token" {
        t.Fatalf("token contract lost: %+v",rule)
    }
    for _,tc:=range []struct{name,source string}{
        {"missing-provider-pointer",strings.Replace(valid,"  result_token_pointer = \"/structuredContent/record/operation_token\"\n","",1)},
        {"missing-write-argument",strings.Replace(valid,"  write_token_argument = \"operation_token\"\n","",1)},
        {"invalid-argument-name",strings.Replace(valid,"operation_token\"","a.b\" ",1)},
        {"invalid-argument-prefix",strings.Replace(valid,"operation_token\"","1token\" ",1)},
        {"invalid-result-pointer",strings.Replace(valid,"result_token_pointer = \"/structuredContent/record/operation_token\"","result_token_pointer = \"/record/~2bad\"",1)},
        {"conflicting-token-contract",valid+"\n"+strings.Replace(valid,`readback "crm_check"`,`readback "another"`,1)},
    } {
        t.Run(tc.name,func(t *testing.T){
            if _,err:=Decode(example(t)+"\n"+tc.source);err==nil {
                t.Fatalf("invalid token contract accepted: %s",tc.name)
            }
        })
    }
}
