package server

import (
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestProtectedTraceAPIValidatesBoundedExactSelectors(t *testing.T) {
    svc,server,_:=fixture(t)
    for _,row:=range []storage.Trace{
        {RunID:"alpha",Node:"first",Status:"success",Message:"done"},
        {RunID:"beta",Node:"foreign",Status:"failure",Message:"other"},
        {RunID:"alpha",Node:"second",Status:"failure",Message:"retry"},
        {RunID:"alpha",Node:"last",Status:"failure",Message:"latest"},
    } {svc.Store.WriteTrace(row)}
    get:=func(path,token string)(int,[]storage.Trace){
        t.Helper()
        req,err:=http.NewRequest("GET",server.URL+path,nil)
        if err!=nil {t.Fatal(err)}
        if token!="" {req.Header.Set("Authorization","Bearer "+token)}
        response,err:=http.DefaultClient.Do(req)
        if err!=nil {t.Fatal(err)}
        defer response.Body.Close()
        if response.StatusCode!=200 {return response.StatusCode,nil}
        var got []storage.Trace
        if err=json.NewDecoder(response.Body).Decode(&got);err!=nil {t.Fatal(err)}
        return response.StatusCode,got
    }
    if status,_:=get("/api/traces?run_id=alpha","");status!=401 {
        t.Fatalf("unauthorized trace read accepted: %d",status)
    }
    status,got:=get("/api/traces?run_id=alpha&status=failure&limit=1","test-secret")
    if status!=200 || len(got)!=1 || got[0].Node!="last" || got[0].Message!="latest" {
        t.Fatalf("filtered admin traces wrong: %d %#v",status,got)
    }
    status,got=get("/api/traces?run_id="+url.QueryEscape("alp")+"&limit=200","test-secret")
    if status!=200 || len(got)!=0 {t.Fatalf("trace query unexpectedly matched partial run: %d %#v",status,got)}
    status,got=get("/api/traces","test-secret")
    if status!=200 || len(got)!=4 {t.Fatalf("legacy trace read broken: %d %#v",status,got)}
    for _,query:=range []string{
        "?limit=0","?limit=-1","?limit=2001","?limit=bad","?limit=",
        "?run_id=x&run_id=y","?status=success&status=failure",
        "?run_id="+strings.Repeat("x",129),
        "?status="+strings.Repeat("s",49),
        "?unexpected=x",
    }{
        t.Run(fmt.Sprintf("invalid-%s",query),func(t *testing.T){
            if status,_:=get("/api/traces"+query,"test-secret");status!=400 {
                t.Fatalf("ambiguous or unbounded trace query %q returned %d",query,status)
            }
        })
    }
}
