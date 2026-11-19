package server

import (
    "fmt"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
    "time"
)

func TestPeerQuotaSameIPDifferentPortsAndIndependentPeers(t *testing.T) {
    limiter,err:=newPeerRateLimiter(2,2)
    if err!=nil {t.Fatal(err)}
    current:=time.Unix(1000,0)
    limiter.now=func()time.Time{return current}
    if !limiter.allow("127.0.0.1:5555") || !limiter.allow("127.0.0.1:5556") {
        t.Fatal("initial burst rejected")
    }
    if limiter.allow("127.0.0.1:5557") {t.Fatal("rotating TCP port bypassed IP budget")}
    if !limiter.allow("127.0.0.2:5555") {t.Fatal("unrelated peer was starved")}
    current=current.Add(500*time.Millisecond)
    if !limiter.allow("127.0.0.1:1") || limiter.allow("127.0.0.1:2") {
        t.Fatal("token refill not monotone")
    }
    if limiter.allow("unparseable-remote") {t.Fatal("malformed remote admitted")}
    if len(limiter.peers)!=2 {t.Fatalf("raw socket ports materialized distinct buckets: %d",len(limiter.peers))}
}

func TestRateAdmissionNeverDispatchesDeniedPublicCallsOrConsumesAdminQuota(t *testing.T) {
    public,err:=newPeerRateLimiter(1,1)
    if err!=nil {t.Fatal(err)}
    admin,err:=newPeerRateLimiter(1,1)
    if err!=nil {t.Fatal(err)}
    o:=&httpObservability{
        counts:map[requestMetricKey]requestMetric{},
        publicSlots:make(chan struct{},4),adminSlots:make(chan struct{},2),
        publicRate:public,adminRate:admin,
    }
    calls:=0
    handler:=o.wrap(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        calls++
        w.WriteHeader(http.StatusNoContent)
    }))
    request:=func(route,forwarded string)int{
        r:=httptest.NewRequest(http.MethodPost,route,strings.NewReader("{}"))
        r.RemoteAddr="198.51.100.10:5555"
        r.Header.Set("X-Forwarded-For",forwarded)
        w:=httptest.NewRecorder()
        handler.ServeHTTP(w,r)
        return w.Code
    }
    if status:=request("/mcp/alpha","192.0.2.2");status!=204{t.Fatal(status)}
    if status:=request("/mcp/beta","192.0.2.3");status!=429{t.Fatal("spoofed forwarded IP bypassed per-peer limit",status)}
    if calls!=1 {t.Fatalf("denied public requests reached handler: %d",calls)}
    if status:=request("/api/status","192.0.2.4");status!=204 {
        t.Fatalf("rate limited public peer consumed admin budget: %d",status)
    }
    if status:=request("/api/status","192.0.2.5");status!=429 {
        t.Fatalf("admin peer rate limit not enforced: %d",status)
    }
    if calls!=2 {t.Fatal("denied admin request reached protected application")}
    for _,record:=range o.Snapshot()["requests"].([]requestMetric) {
        if record.Route=="public_mcp" && record.Status=="429" && record.Count==1 {return}
    }
    t.Fatal("public throttle counter missing")
}

func TestRateQuotasBoundTrackingAndRejectMalformedConfiguration(t *testing.T) {
    l,err:=newPeerRateLimiter(1,1)
    if err!=nil {t.Fatal(err)}
    for n:=0;n<maxRatePeers;n++ {
        address:=fmt.Sprintf("10.%d.%d.%d:100",n/65536,(n/256)%256,n%256)
        if !l.allow(address){t.Fatalf("legitimate new peer %d denied before capacity",n)}
    }
    if l.allow("11.0.0.1:100") {t.Fatal("unbounded new-peer allocation accepted")}
    if len(l.peers)!=maxRatePeers {t.Fatal("peer cache grew without bound")}
    t.Setenv("REGATE_PUBLIC_RPS","nonsense")
    if _,err:=newHTTPObservability();err==nil {t.Fatal("invalid rate configuration did not fail closed")}
    t.Setenv("REGATE_PUBLIC_RPS","100")
    t.Setenv("REGATE_ADMIN_BURST","0")
    if _,err:=newHTTPObservability();err==nil {t.Fatal("zero admin burst permitted")}
}

func TestCollectorQueueOutageIsBoundedAndCountersRemainPrivate(t *testing.T) {
    exporter:=&otlpTraceExporter{queue:make(chan otlpHTTPSpan,2)}
    o:=&httpObservability{counts:map[requestMetricKey]requestMetric{},
        publicSlots:make(chan struct{},2),adminSlots:make(chan struct{},2),
        exporter:exporter,
    }
    h:=o.wrap(http.HandlerFunc(func(w http.ResponseWriter,_ *http.Request) {w.WriteHeader(204)}))
    for i:=0;i<12;i++ {
        response:=httptest.NewRecorder()
        request:=httptest.NewRequest("GET","/mcp/test?never-log-sensitive",nil)
        h.ServeHTTP(response,request)
        if response.Code!=204 {t.Fatal("collector outage blocked safe handler")}
    }
    if got:=exporter.dropped.Load();got!=10 {t.Fatalf("expected 10 dropped spans, got %d",got)}
    rows:=o.Snapshot()["requests"].([]requestMetric)
    if len(rows)!=1 || rows[0].Route!="public_mcp" || rows[0].Count!=12 {
        t.Fatalf("unbounded metric cardinality or missing request: %#v",rows)
    }
}
