package server

import (
    "bufio"
    "bytes"
    "context"
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net"
    "net/http"
    "net/url"
    "os"
    "sort"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/burkostya/regate/internal/netguard"
)

// Only these finite, non-user-controlled dimensions are retained. Never log
// the URL path, query, headers, MCP method, tool, principal or request body.
type requestMetricKey struct {
    Route string `json:"route"`
    Method string `json:"method"`
    Status string `json:"status"`
}
type requestMetric struct {
    requestMetricKey
    Count uint64 `json:"count"`
    DurationUS uint64 `json:"duration_us_total"`
}

type httpObservability struct {
    mu sync.Mutex
    counts map[requestMetricKey]requestMetric
    publicSlots chan struct{}
    adminSlots chan struct{}
    publicRate *peerRateLimiter
    adminRate *peerRateLimiter
    exporter *otlpTraceExporter
}

const maxInboundBodyBytes int64 = 4 << 20

// Separate admission pools prevent saturation of public MCP sessions from
// starving recovery/management operations. Limits are process-local, not a
// substitute for a reverse proxy's per-tenant rate limit.
func newHTTPObservability() (*httpObservability, error) {
    o:=&httpObservability{
        counts:make(map[requestMetricKey]requestMetric),
        publicSlots:make(chan struct{},128),
        adminSlots:make(chan struct{},16),
    }
    var err error
    o.publicRate,err=rateLimiterFromEnv("PUBLIC",300,600)
    if err!=nil {return nil,err}
    o.adminRate,err=rateLimiterFromEnv("ADMIN",100,200)
    if err!=nil {return nil,err}
    endpoint:=strings.TrimSpace(os.Getenv("REGATE_OTLP_TRACES_URL"))
    if endpoint!="" {
        u,err:=url.Parse(endpoint)
        if err!=nil || u.RawQuery!="" || u.Fragment!="" || !strings.HasSuffix(u.Path,"/v1/traces") {
            return nil,errors.New("REGATE_OTLP_TRACES_URL must be an absolute, query-free OTLP /v1/traces URL")
        }
        if err=netguard.ValidateURL(endpoint);err!=nil {
            return nil,fmt.Errorf("REGATE_OTLP_TRACES_URL rejected: %w",err)
        }
        o.exporter=&otlpTraceExporter{
            endpoint:endpoint, client:netguard.Client(2*time.Second),
            queue:make(chan otlpHTTPSpan,256),
        }
    }
    return o,nil
}

func stableHTTPRoute(path string) string {
    switch {
    case path=="/healthz": return "health"
    case path=="/": return "index"
    case path=="/mcp/admin": return "admin_mcp"
    case strings.HasPrefix(path,"/api/"): return "admin_api"
    case strings.HasPrefix(path,"/.well-known/oauth-protected-resource/"): return "oauth_metadata"
    case strings.HasPrefix(path,"/mcp/"): return "public_mcp"
    default: return "not_found"
    }
}

func stableHTTPMethod(method string) string {
    switch method {
    case "GET","POST","PUT","DELETE","OPTIONS","HEAD","PATCH": return method
    default: return "OTHER"
    }
}

func stableHTTPStatus(code int) string {
    switch code {
    case 200,202,204,400,401,403,404,405,409,413,415,429,500,503: return fmt.Sprint(code)
    }
    switch {
    case code<300: return "other_2xx"
    case code<400: return "other_3xx"
    case code<500: return "other_4xx"
    default: return "other_5xx"
    }
}

type statusObserver struct {
    http.ResponseWriter
    status int
}
func (w *statusObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statusObserver) WriteHeader(status int) {
    if w.status==0 {w.status=status}
    w.ResponseWriter.WriteHeader(status)
}
func (w *statusObserver) Write(p []byte)(int,error) {
    if w.status==0 {w.status=http.StatusOK}
    return w.ResponseWriter.Write(p)
}
// The official MCP SDK uses Flusher for Streamable HTTP and subscriptions.
// Preserve streaming and hijacking when observing response status.
func (w *statusObserver) Flush() {
    if w.status==0 {w.status=http.StatusOK}
    _=http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *statusObserver) Hijack()(net.Conn,*bufio.ReadWriter,error) {
    return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (o *httpObservability) wrap(next http.Handler) http.Handler {
    if o==nil {return next}
    return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        started:=time.Now()
        route:=stableHTTPRoute(r.URL.Path)
        method:=stableHTTPMethod(r.Method)
        writer:=&statusObserver{ResponseWriter:w}
        slots:=o.publicSlots
        if route=="admin_api" || route=="admin_mcp" {slots=o.adminSlots}
        limiter:=o.publicRate
        if route=="admin_api" || route=="admin_mcp" {limiter=o.adminRate}
        if !limiter.allow(r.RemoteAddr) {
            w.Header().Set("Retry-After","1")
            http.Error(writer,"peer request budget exceeded",http.StatusTooManyRequests)
            o.observe(route,method,writer.status,started)
            return
        }
        select {
        case slots<-struct{}{}:
            defer func(){ <-slots }()
        default:
            w.Header().Set("Retry-After","1")
            http.Error(writer,"request admission capacity exceeded",http.StatusServiceUnavailable)
            o.observe(route,method,writer.status,started)
            return
        }
        if r.ContentLength>maxInboundBodyBytes {
            http.Error(writer,"request body exceeds 4 MiB",http.StatusRequestEntityTooLarge)
            o.observe(route,method,writer.status,started)
            return
        }
        if r.Body!=nil {r.Body=http.MaxBytesReader(writer,r.Body,maxInboundBodyBytes)}
        next.ServeHTTP(writer,r)
        if writer.status==0 {writer.status=http.StatusOK}
        o.observe(route,method,writer.status,started)
    })
}

func (o *httpObservability) observe(route,method string,status int,started time.Time) {
    duration:=time.Since(started)
    key:=requestMetricKey{route,method,stableHTTPStatus(status)}
    o.mu.Lock()
    metric:=o.counts[key]
    metric.requestMetricKey=key
    metric.Count++
    metric.DurationUS+=uint64(duration.Microseconds())
    o.counts[key]=metric
    o.mu.Unlock()
    if o.exporter!=nil {
        var id [24]byte
        if _,err:=rand.Read(id[:]);err==nil {
            o.exporter.offer(otlpHTTPSpan{
                traceID:hex.EncodeToString(id[:16]),spanID:hex.EncodeToString(id[16:]),
                route:route,method:method,status:status,started:started,end:time.Now(),
            })
        }
    }
}

func (o *httpObservability) Snapshot() map[string]any {
    o.mu.Lock()
    records:=make([]requestMetric,0,len(o.counts))
    for _,value:=range o.counts {records=append(records,value)}
    o.mu.Unlock()
    sort.Slice(records,func(i,j int)bool{
        a,b:=records[i],records[j]
        return a.Route+"|"+a.Method+"|"+a.Status<b.Route+"|"+b.Method+"|"+b.Status
    })
    var dropped uint64
    if o.exporter!=nil {dropped=o.exporter.dropped.Load()}
    return map[string]any{
        "requests":records,
        "active_public_requests":len(o.publicSlots),
        "active_admin_requests":len(o.adminSlots),
        "otlp_spans_dropped":dropped,
        "otlp_enabled":o.exporter!=nil,
    }
}

// OTLP/HTTP JSON trace transport, with no raw input-derived attributes.
// A fixed-size queue and a single bounded worker prevent a downed telemetry
// collector from blocking or creating unbounded goroutines during MCP calls.
type otlpHTTPSpan struct {
    traceID,spanID,route,method string
    status int
    started,end time.Time
}
type otlpTraceExporter struct {
    endpoint string
    client *http.Client
    queue chan otlpHTTPSpan
    dropped atomic.Uint64
}
func (e *otlpTraceExporter) offer(span otlpHTTPSpan) {
    select {
    case e.queue<-span:
    default: e.dropped.Add(1)
    }
}
func (e *otlpTraceExporter) run(ctx context.Context) {
    tick:=time.NewTicker(time.Second)
    defer tick.Stop()
    batch:=make([]otlpHTTPSpan,0,32)
    flush:=func(parent context.Context){
        if len(batch)==0{return}
        exportCtx,cancel:=context.WithTimeout(parent,2*time.Second)
        defer cancel()
        if err:=e.exportBatch(exportCtx,batch);err!=nil {e.dropped.Add(uint64(len(batch)))}
        batch=batch[:0]
    }
    for {
        select {
        case span:=<-e.queue:
            batch=append(batch,span)
            if len(batch)>=32 {flush(ctx)}
        case <-tick.C: flush(ctx)
        case <-ctx.Done():
            // Shutdown is best effort and strictly bounded. Never let an
            // exporter failure prolong the server shutdown indefinitely.
            drain:=true
            for drain && len(batch)<32 {
                select {case span:=<-e.queue: batch=append(batch,span);default:drain=false}
            }
            flush(context.Background())
            return
        }
    }
}
func otlpStringAttribute(key,value string) map[string]any {
    return map[string]any{"key":key,"value":map[string]any{"stringValue":value}}
}
func (e *otlpTraceExporter) exportBatch(ctx context.Context,spans []otlpHTTPSpan) error {
    if len(spans)==0{return nil}
    records:=make([]any,0,len(spans))
    for _,s:=range spans {
        records=append(records,map[string]any{
            "traceId":s.traceID,"spanId":s.spanID,
            "name":"HTTP "+s.method+" "+s.route,
            "kind":2, // OTLP SpanKind SERVER
            "startTimeUnixNano":fmt.Sprint(s.started.UnixNano()),
            "endTimeUnixNano":fmt.Sprint(s.end.UnixNano()),
            "attributes":[]any{
                otlpStringAttribute("http.route",s.route),
                otlpStringAttribute("http.request.method",s.method),
                map[string]any{"key":"http.response.status_code","value":map[string]any{"intValue":fmt.Sprint(s.status)}},
            },
        })
    }
    payload:=map[string]any{"resourceSpans":[]any{map[string]any{
        "resource":map[string]any{"attributes":[]any{otlpStringAttribute("service.name","regate")}},
        "scopeSpans":[]any{map[string]any{
            "scope":map[string]any{"name":"regate.http"},
            "spans":records,
        }},
    }}}
    raw,err:=json.Marshal(payload)
    if err!=nil{return err}
    request,err:=http.NewRequestWithContext(ctx,http.MethodPost,e.endpoint,bytes.NewReader(raw))
    if err!=nil{return err}
    request.Header.Set("Content-Type","application/json")
    response,err:=e.client.Do(request)
    if err!=nil{return err}
    defer response.Body.Close()
    _,_=io.Copy(io.Discard,io.LimitReader(response.Body,1024))
    if response.StatusCode<200 || response.StatusCode>=300 {
        return fmt.Errorf("collector returned HTTP %d",response.StatusCode)
    }
    return nil
}
