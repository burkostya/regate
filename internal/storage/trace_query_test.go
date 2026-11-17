package storage

import (
    "fmt"
    "reflect"
    "testing"
)

func TestTraceQueryExactRunStatusAndLimitAfterFiltering(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil {t.Fatal(err)}
    defer s.Close()
    for _,row:=range []Trace{
        {RunID:"run-1",Status:"success",Node:"old"},
        {RunID:"run-2",Status:"failure",Node:"other"},
        {RunID:"run-1",Status:"failure",Node:"middle"},
        {RunID:"run-2",Status:"failure",Node:"newer"},
        {RunID:"run-1",Status:"failure",Node:"newest"},
    } {s.WriteTrace(row)}
    query:=s.TracesFiltered(TraceQuery{RunID:"run-1",Status:"failure",Limit:2})
    nodes:=[]string{}
    for _,v:=range query {nodes=append(nodes,v.Node)}
    if !reflect.DeepEqual(nodes,[]string{"newest","middle"}) {
        t.Fatalf("trace limit applied before filtering or order changed: %v",nodes)
    }
    if got:=s.TracesFiltered(TraceQuery{RunID:"run-1",Status:"denied",Limit:100});len(got)!=0 {
        t.Fatalf("unexpected status match: %#v",got)
    }
    if got:=s.TracesFiltered(TraceQuery{RunID:"run",Limit:100});len(got)!=0 {
        t.Fatalf("prefix run matched exact-run filter: %#v",got)
    }
    if got:=s.Traces();len(got)!=5 || got[0].Node!="newest" || got[4].Node!="old" {
        t.Fatalf("unfiltered legacy trace API changed: %#v",got)
    }
}

func TestTraceQueryLimitedByRetainedProjection(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil {t.Fatal(err)}
    defer s.Close()
    for i:=0;i<2010;i++ {
        s.WriteTrace(Trace{RunID:"r",Node:fmt.Sprintf("n%d",i),Status:"success"})
    }
    result:=s.TracesFiltered(TraceQuery{RunID:"r",Limit:2010})
    if len(result)!=2000 || result[0].Node!="n2009" || result[len(result)-1].Node!="n10" {
        t.Fatalf("bounded retained trace projection incorrect: len=%d top=%s last=%s",len(result),result[0].Node,result[len(result)-1].Node)
    }
}
