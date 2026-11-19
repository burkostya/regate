package server

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func TestHTTPMetricsPrivateAndFixedCardinality(t *testing.T) {
    svc,server,_:=fixture(t)
    response,err:=http.Get(server.URL+"/api/metrics")
    if err!=nil {t.Fatal(err)}
    response.Body.Close()
    if response.StatusCode!=http.StatusUnauthorized {t.Fatalf("anonymous metrics status %d",response.StatusCode)}
    req,_:=http.NewRequest("GET",server.URL+"/api/metrics?token=NEVER_LOG_THIS",nil)
    req.Header.Set("Authorization","Bearer test-secret")
    response,err=http.DefaultClient.Do(req)
    if err!=nil {t.Fatal(err)}
    defer response.Body.Close()
    if response.StatusCode!=http.StatusOK {t.Fatalf("authorized metrics status %d",response.StatusCode)}
    payload,err:=io.ReadAll(response.Body)
    if err!=nil {t.Fatal(err)}
    if strings.Contains(string(payload),"NEVER_LOG_THIS") || strings.Contains(string(payload),"test-secret") {
        t.Fatalf("sensitive URL/header leaked to telemetry: %s",payload)
    }
    var metrics struct{Requests []requestMetric `json:"requests"`}
    if err=json.Unmarshal(payload,&metrics);err!=nil {t.Fatal(err)}
    found:=false
    for _,record:=range metrics.Requests {
        if record.Route=="admin_api" && record.Status=="401" && record.Count>0 {found=true}
    }
    if !found {t.Fatalf("missing unauthorized counter: %#v",metrics.Requests)}
    if svc.observability==nil {t.Fatal("observability not configured")}
}

func TestHTTPAdmissionDoesNotStarveAdminOrBreakStreaming(t *testing.T) {
    o:=&httpObservability{
        counts:map[requestMetricKey]requestMetric{},
        publicSlots:make(chan struct{},1),
        adminSlots:make(chan struct{},1),
    }
    started:=make(chan struct{})
    release:=make(chan struct{})
    h:=o.wrap(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        if r.URL.Path=="/mcp/public" {
            close(started)
            <-release
        }
        if _,ok:=w.(http.Flusher);!ok {t.Error("MCP SSE Flusher lost")}
        w.Header().Set("Content-Type","text/event-stream")
        w.WriteHeader(200)
        w.(http.Flusher).Flush()
    }))
    done:=make(chan struct{})
    go func(){h.ServeHTTP(httptest.NewRecorder(),httptest.NewRequest("GET","/mcp/public",nil));close(done)}()
    <-started
    overloaded:=httptest.NewRecorder()
    h.ServeHTTP(overloaded,httptest.NewRequest("GET","/mcp/other",nil))
    if overloaded.Code!=503 {t.Fatalf("public capacity not enforced: %d",overloaded.Code)}
    admin:=httptest.NewRecorder()
    h.ServeHTTP(admin,httptest.NewRequest("GET","/api/status",nil))
    if admin.Code!=200 {t.Fatalf("admin recovery starved: %d",admin.Code)}
    close(release)
    <-done
    snapshot:=o.Snapshot()
    rows:=snapshot["requests"].([]requestMetric)
    found:=false
    for _,r:=range rows {if r.Route=="public_mcp" && r.Status=="503"{found=true}}
    if !found {t.Fatalf("missing overload metric: %#v",rows)}
}

func TestHTTPBodyCeilingWithoutEffectDispatch(t *testing.T) {
    o:=&httpObservability{counts:map[requestMetricKey]requestMetric{},publicSlots:make(chan struct{},2),adminSlots:make(chan struct{},2)}
    calls:=0
    h:=o.wrap(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        calls++
        _,err:=io.ReadAll(r.Body)
        if err==nil {w.WriteHeader(http.StatusNoContent)}
    }))
    tooBig:=httptest.NewRequest("POST","/mcp/test",nil)
    tooBig.ContentLength=maxInboundBodyBytes+1
    rec:=httptest.NewRecorder()
    h.ServeHTTP(rec,tooBig)
    if rec.Code!=http.StatusRequestEntityTooLarge || calls!=0 {t.Fatalf("oversized request reached handler: code=%d calls=%d",rec.Code,calls)}
    // Chunked streams with unknown content length are bounded as well.
    streamed:=httptest.NewRequest("POST","/api/run",strings.NewReader(strings.Repeat("x",int(maxInboundBodyBytes)+1)))
    streamed.ContentLength=-1
    rec=httptest.NewRecorder()
    h.ServeHTTP(rec,streamed)
    if calls!=1 || rec.Code==204 {t.Fatalf("unbounded streaming request accepted: %d",rec.Code)}
}

func TestOTLPTraceExportContainsOnlyBoundedLabels(t *testing.T) {
    var exported []byte
    collector:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        if r.URL.Path!="/v1/traces" || r.Header.Get("Content-Type")!="application/json" {
            t.Errorf("unexpected export: %s %s",r.URL.Path,r.Header.Get("Content-Type"))
        }
        exported,_=io.ReadAll(r.Body)
        w.WriteHeader(200)
    }))
    defer collector.Close()
    t.Setenv("REGATE_OTLP_TRACES_URL",collector.URL+"/v1/traces")
    o,err:=newHTTPObservability()
    if err!=nil {t.Fatal(err)}
    now:=time.Now()
    s:=otlpHTTPSpan{
        traceID:strings.Repeat("a",32),spanID:strings.Repeat("b",16),
        route:"public_mcp",method:"POST",status:403,started:now,end:now.Add(time.Millisecond),
    }
    if err:=o.exporter.exportBatch(context.Background(),[]otlpHTTPSpan{s});err!=nil {t.Fatal(err)}
    if !json.Valid(exported){t.Fatal("invalid OTLP JSON")}
    for _,text:=range []string{"service.name","traceId","spanId","http.route","public_mcp","http.response.status_code"} {
        if !strings.Contains(string(exported),text){t.Fatalf("missing %s in %s",text,exported)}
    }
    for _,secret:=range []string{"Authorization","Bearer","input","arguments","token","customer","url.path"} {
        if strings.Contains(string(exported),secret) {t.Fatalf("potential secret attribute %s: %s",secret,exported)}
    }
}

func TestOTLPConfigurationRejectsUnapprovedEgress(t *testing.T) {
    t.Setenv("REGATE_OTLP_TRACES_URL","http://169.254.169.254/v1/traces")
    if _,err:=newHTTPObservability();err==nil {t.Fatal("metadata-service destination accepted")}
    t.Setenv("REGATE_OTLP_TRACES_URL","https://unapproved.invalid/v1/traces")
    if _,err:=newHTTPObservability();err==nil {t.Fatal("non-allowlisted collector accepted")}
}
