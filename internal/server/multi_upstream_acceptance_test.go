package server

import (
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "sync/atomic"
    "testing"
)

// Exercises the real Regate transport, policy/effect engine, typed pipeline,
// and TWO independently running HTTP MCP fixtures. It deliberately does not
// pretend that loopback fixtures verify production external provider behavior.
func TestTwoUpstreamCuratedSurfacesDecisionAndAgentDataBoundary(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","two-upstream-admin")
    crmWorld:=&mockWorld{}
    agentWorld:=&mockWorld{}
    var decisions atomic.Int64
    // The policy hook and CRM tool share a server in this fixture; the MCP
    // agent has an independently hosted MCP endpoint and isolated call log.
    crm:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
        if r.URL.Path=="/decision" {decisions.Add(1)}
        crmWorld.ServeHTTP(w,r)
    }))
    defer crm.Close()
    agent:=httptest.NewServer(agentWorld)
    defer agent.Close()
    if crm.URL==agent.URL {t.Fatal("independent HTTP MCP endpoints unexpectedly coincide")}
    original,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    source:=strings.Replace(string(original),"http://127.0.0.1:9091/mcp",crm.URL+"/mcp",1)
    source=strings.Replace(source,"http://127.0.0.1:9091/mcp",agent.URL+"/mcp",1)
    source=strings.ReplaceAll(source,"http://127.0.0.1:9091/decision",crm.URL+"/decision")
    source=strings.ReplaceAll(source,"http://127.0.0.1:9091/redact",crm.URL+"/redact")
    if strings.Contains(source,"http://127.0.0.1:9091") {t.Fatal("fixture did not replace all upstream URLs")}
    source+=`
surface "agent_only" {
  path = "/mcp/agent_only"
  tool "summarize_notes" {
    source = "agent.summarize"
    description = "Independent curated agent-only MCP surface"
    read_only = true
  }
  resource "guide" {
    uri = "regate://agent_only/guide"
    mime_type = "text/plain"
    text = "Agent-only instructions"
  }
  prompt "safe_summary" {
    description = "Review redacted notes"
    text = "Summarize redacted notes without identifiers."
  }
}
`
    svc,err:=New(t.TempDir(),source)
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()

    listNames:=func(surface,method,key string) []string {
        t.Helper()
        result:=decode(t,latestCall(t,api.URL+"/mcp/"+surface,"",method,50,map[string]any{}))
        if result["error"]!=nil {t.Fatalf("%s: %v",method,result)}
        entries,ok:=result["result"].(map[string]any)[key].([]any)
        if !ok {t.Fatalf("missing %s in %v",key,result)}
        names:=make([]string,0,len(entries))
        for _,entry:=range entries {
            names=append(names,entry.(map[string]any)["name"].(string))
        }
        return names
    }
    support:=listNames("support","tools/list","tools")
    agentTools:=listNames("agent_only","tools/list","tools")
    if len(support)!=2 || len(agentTools)!=1 || agentTools[0]!="summarize_notes" {
        t.Fatalf("cross-surface tool leakage: support=%v agent=%v",support,agentTools)
    }
    if resources:=listNames("agent_only","resources/list","resources");len(resources)!=1 || resources[0]!="guide" {
        t.Fatalf("agent resource descriptor not curated: %v",resources)
    }
    if prompts:=listNames("agent_only","prompts/list","prompts");len(prompts)!=1 || prompts[0]!="safe_summary" {
        t.Fatalf("agent prompt descriptor not curated: %v",prompts)
    }

    call:=func(customerID string) map[string]any {
        t.Helper()
        reply:=decode(t,latestCall(t,api.URL+"/mcp/support","","tools/call",51,
            map[string]any{"name":"summarize_customer","arguments":map[string]any{"customer_id":customerID}}))
        if reply["error"]!=nil {t.Fatalf("tool transport failure %v",reply)}
        return reply["result"].(map[string]any)
    }
    allowed:=call("C123")
    if allowed["isError"]==true {t.Fatalf("authorized composition failed: %v",allowed)}
    wire,err:=json.Marshal(allowed)
    if err!=nil {t.Fatal(err)}
    if strings.Contains(string(wire),"alice@example.com") {
        t.Fatal("unredacted CRM data published by composed MCP tool")
    }
    agentWorld.mu.Lock()
    agentInput:=agentWorld.agentInput
    agentWorld.mu.Unlock()
    if agentInput==nil || agentInput["email"]!="[REDACTED]" {
        t.Fatalf("agent received unmasked CRM input: %#v",agentInput)
    }
    countToolCalls:=func(world *mockWorld)int {
        world.mu.Lock()
        defer world.mu.Unlock()
        count:=0
        for _,method:=range world.calls {if method=="tools/call" {count++}}
        return count
    }
    crmBefore,agentBefore:=countToolCalls(crmWorld),countToolCalls(agentWorld)
    if crmBefore!=1 || agentBefore!=1 {
        t.Fatalf("expected exactly one CRM and one agent tool call: crm=%d agent=%d",crmBefore,agentBefore)
    }
    denied:=call("DENY-123")
    if denied["isError"]!=true {t.Fatalf("Decision API denial was ignored: %#v",denied)}
    if countToolCalls(crmWorld)!=crmBefore || countToolCalls(agentWorld)!=agentBefore {
        t.Fatal("denied request dispatched to protected CRM or agent")
    }
    if decisions.Load()!=2 {t.Fatalf("expected both requests to pass Decision API: %d",decisions.Load())}
    if len(svc.Store.Traces())<4 {t.Fatal("composed execution lacked node traces")}
    // Prove opaque provider responses are routed over actual network HTTP:
    // all protocol and effect calls were executed by the two live handlers.
    for _,world:=range []*mockWorld{crmWorld,agentWorld} {
        world.mu.Lock()
        methods:=append([]string{},world.calls...)
        world.mu.Unlock()
        if len(methods)<2 {t.Fatalf("MCP provider was not initialized over HTTP: %v",methods)}
    }

    // The active source is immutable; exercise the independent agent catalog
    // without exposing raw credentials or any management operations.
    resp:=latestCall(t,api.URL+"/mcp/agent_only","","resources/read",52,
        map[string]any{"uri":"regate://agent_only/guide"})
    defer resp.Body.Close()
    raw,_:=io.ReadAll(resp.Body)
    if resp.StatusCode!=http.StatusOK || !strings.Contains(string(raw),"Agent-only instructions") {
        t.Fatalf("independent surface resource not readable: HTTP %d: %s",resp.StatusCode,raw)
    }
}
