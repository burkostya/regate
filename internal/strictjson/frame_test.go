package strictjson

import (
    "strings"
    "testing"
)

func TestReadObjectStrictFraming(t *testing.T) {
    good:=[]string{
        `{"decision":"allow"}`,
        `{"decision":"allow","detail":{"one":1,"two":[true,null]}}   \n`,
    }
    for _,raw:=range good {
        if v,err:=ReadObject(strings.NewReader(raw),1024);err!=nil || v["decision"]!="allow" {
            t.Fatalf("valid JSON object rejected: %#v %v",v,err)
        }
    }
    bad:=[]string{
        `{"decision":"deny","decision":"allow"}`,
        `{"decision":"deny","deci\u0073ion":"allow"}`,
        `{"decision":"allow","metadata":{"token":"one","token":"two"}}`,
        `{"decision":"allow"}{"decision":"deny"}`,
        `{"decision":"allow"}invalid`,
        "null","[]",
        `{"decision":"allow","text":"`+string([]byte{0xff})+`"}`,
        strings.Repeat("[",MaxDepth+1)+"0"+strings.Repeat("]",MaxDepth+1),
    }
    for _,raw:=range bad {
        if _,err:=ReadObject(strings.NewReader(raw),1024);err==nil {
            t.Fatalf("ambiguous or invalid JSON accepted: %q",raw)
        }
    }
}
func TestReadObjectChecksEntireBodyAtSizeBoundary(t *testing.T) {
    const max = 256
    prefix:=`{"padding":"`
    suffix:=`"}`
    raw:=prefix+strings.Repeat("x",max-len(prefix)-len(suffix))+suffix
    if _,err:=ReadObject(strings.NewReader(raw),max);err!=nil {
        t.Fatalf("exact maximum JSON length rejected: %v",err)
    }
    for _,invalid:=range []string{
        raw+" ",`{"decision":"allow"}`+strings.Repeat(" ",max),
    } {
        if _,err:=ReadObject(strings.NewReader(invalid),max);err==nil {
            t.Fatal("oversized valid JSON prefix accepted")
        }
    }
}
