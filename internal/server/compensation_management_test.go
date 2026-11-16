package server

import (
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestManagementCompensationRequiresAdminAndVerifiedAppliedSource(t *testing.T){
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    world:=&mockWorld{}
    mock:=httptest.NewServer(world)
    defer mock.Close()
    raw,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil{t.Fatal(err)}
    source:=strings.ReplaceAll(string(raw),"http://127.0.0.1:9091",mock.URL)+`
compensation "undo_customer" {
  for_operation = "crm.create"
  upstream = "crm"
  tool = "delete_customer"
  intent_pointer = "/customer_id"
  argument = "customer_id"
  enabled = true
}
`
    svc,err:=New(t.TempDir(),source)
    if err!=nil{t.Fatal(err)}
    defer svc.Store.Close()
    api:=httptest.NewServer(svc.Handler())
    defer api.Close()
    unknown:=func(run string) storage.Effect{
        t.Helper()
        effect,err:=svc.Store.BeginEffectBound(run,"create","crm.create",
            storage.TargetDigest("mcp",mock.URL+"/mcp"),
            map[string]any{"customer_id":"C123"})
        if err!=nil{t.Fatal(err)}
        if err=svc.Store.CompleteEffect(effect.ID,"unknown","lost");err!=nil{t.Fatal(err)}
        return effect
    }
    original:=unknown("comp-rest")
    args:=map[string]any{
        "effect_id":original.ID,
        "contract":"undo_customer",
        "intent":map[string]any{"customer_id":"C123"},
        "actor":"operator-42",
        "reason":"verified-applied creation and approved inverse",
    }
    denied:=jsonPost(t,api.URL+"/api/effect-compensate","",args)
    denied.Body.Close()
    if denied.StatusCode!=http.StatusUnauthorized {t.Fatal("anonymous compensation allowed")}
    unverified:=jsonPost(t,api.URL+"/api/effect-compensate","test-secret",args)
    unverified.Body.Close()
    if unverified.StatusCode!=http.StatusBadRequest {t.Fatal("unresolved original was compensated")}
    _,err=svc.Store.ResolveByOperator(original.ID,"reviewer","applied","matched remote record")
    if err!=nil{t.Fatal(err)}
    payload:=decode(t,jsonPost(t,api.URL+"/api/effect-compensate","test-secret",args))
    if payload["status"]!="confirmed" || payload["compensates"]!=original.ID {
        t.Fatalf("compensation journal missing: %#v",payload)
    }
    approval,ok:=payload["compensation_approval"].(map[string]any)
    if !ok || approval["actor"]!="operator-42" || len(approval["reason_digest"].(string))!=64 {
        t.Fatalf("compensation approval missing: %#v",payload)
    }
    repeat:=jsonPost(t,api.URL+"/api/effect-compensate","test-secret",args)
    repeat.Body.Close()
    if repeat.StatusCode!=http.StatusBadRequest {t.Fatal("inverse was replayed")}
    parent,err:=svc.Store.EffectGet(original.ID)
    if err!=nil || parent.Status!="resolved_applied" {
        t.Fatalf("original source mutated: %#v %v",parent,err)
    }
    original2:=unknown("comp-mcp")
    _,err=svc.Store.ResolveByOperator(original2.ID,"reviewer","applied","audit")
    if err!=nil{t.Fatal(err)}
    mcpResponse:=decode(t,latestCall(t,api.URL+"/mcp/admin","test-secret","tools/call",91,
        map[string]any{"name":"regate_effect_compensate","arguments":map[string]any{
            "effect_id":original2.ID,"contract":"undo_customer",
            "intent":map[string]any{"customer_id":"C123"},
            "actor":"reviewer","reason":"authorized",
        }}))
    if mcpResponse["error"]!=nil {t.Fatal(mcpResponse)}
    toolResult:=mcpResponse["result"].(map[string]any)
    if toolResult["isError"]==true {t.Fatalf("MCP compensation failed: %#v",toolResult)}
    stored,err:=svc.Store.EffectGet(storage.CompensationID(original2.ID))
    if err!=nil || stored.Status!="confirmed" || stored.Compensates!=original2.ID {
        t.Fatalf("management MCP compensation not durable: %#v %v",stored,err)
    }
}
