package server

import (
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestManagementMCPReadbackEnforcesAdminAndBoundIntent(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    world:=&mockWorld{}
    mock:=httptest.NewServer(world)
    defer mock.Close()
    raw,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    source:=strings.ReplaceAll(string(raw),"http://127.0.0.1:9091",mock.URL)+
        `
readback "customer_lookup" {
  write_operation = "crm.update_customer"
  upstream = "crm"
  read_tool = "get_customer"
  intent_pointer = "/customer_id"
  read_argument = "customer_id"
  result_pointer = "/structuredContent/customer_id"
  safe_read = true
}
`
    svc,err:=New(t.TempDir(),source)
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()
    createUnknown:=func(run string) storage.Effect {
        t.Helper()
        effect,err:=svc.Store.BeginEffectBound(run,"update","crm.update_customer",
            storage.TargetDigest("mcp",mock.URL+"/mcp"),map[string]any{"customer_id":"C123"})
        if err!=nil {t.Fatal(err)}
        if err=svc.Store.CompleteEffect(effect.ID,"unknown","lost response");err!=nil {t.Fatal(err)}
        return effect
    }
    effect:=createUnknown("run-rest")
    args:=map[string]any{
        "effect_id":effect.ID,
        "adapter":"customer_lookup",
        "intent":map[string]any{"customer_id":"C123"},
    }
    anon:=jsonPost(t,api.URL+"/api/effect-readback","",args)
    anon.Body.Close()
    if anon.StatusCode!=http.StatusUnauthorized {
        t.Fatalf("anonymous readback accepted: %d",anon.StatusCode)
    }
    bad:=map[string]any{
        "effect_id":effect.ID,
        "adapter":"customer_lookup",
        "intent":map[string]any{"customer_id":"CHANGED"},
    }
    unauthenticatedIntent:=jsonPost(t,api.URL+"/api/effect-readback","test-secret",bad)
    unauthenticatedIntent.Body.Close()
    if unauthenticatedIntent.StatusCode!=http.StatusBadRequest {
        t.Fatal("mismatched original writer intent accepted")
    }
    saved,err:=svc.Store.EffectGet(effect.ID)
    if err!=nil || saved.Status!="unknown" {t.Fatalf("rejected attempt was mutated: %#v %v",saved,err)}
    resolved:=decode(t,jsonPost(t,api.URL+"/api/effect-readback","test-secret",args))
    if resolved["status"]!="resolved_applied" {
        t.Fatalf("positive administrative readback failed: %#v",resolved)
    }
    evidence,ok:=resolved["resolution"].(map[string]any)
    if !ok || evidence["kind"]!="readback" || evidence["verifier"]!="hcl.readback:customer_lookup" {
        t.Fatalf("readback evidence misclassified: %#v",resolved)
    }
    effect2:=createUnknown("run-mcp")
    rpc:=decode(t,latestCall(t,api.URL+"/mcp/admin","test-secret","tools/call",89,
        map[string]any{"name":"regate_effect_readback","arguments":map[string]any{
            "effect_id":effect2.ID,
            "adapter":"customer_lookup",
            "intent":map[string]any{"customer_id":"C123"},
        }}))
    if rpc["error"]!=nil {t.Fatal(rpc)}
    returned:=rpc["result"].(map[string]any)
    if returned["isError"]==true {t.Fatalf("readback MCP tool rejected: %#v",returned)}
    current,err:=svc.Store.EffectGet(effect2.ID)
    if err!=nil || current.Status!="resolved_applied" ||
        current.Resolution==nil || current.Resolution.Kind!="readback" {
        t.Fatalf("MCP readback not committed: %#v %v",current,err)
    }
    denied:=jsonPost(t,api.URL+"/api/effect-readback","test-secret",args)
    defer denied.Body.Close()
    if denied.StatusCode!=http.StatusBadRequest {
        t.Fatalf("resolved attempt accepted twice: %d",denied.StatusCode)
    }
}
