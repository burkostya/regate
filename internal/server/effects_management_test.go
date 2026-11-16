package server

import (
    "io"
    "net/http"
    "testing"

)
func TestManagementEffectDecisionRequiresAdminAndUnknownState(t *testing.T){
    svc,api,_:=fixture(t)
    attempt,err:=svc.Store.BeginEffect("run-recovery","create","crm.create",map[string]any{"id":"C100"})
    if err!=nil{t.Fatal(err)}
    if err=svc.Store.CompleteEffect(attempt.ID,"unknown","connection reset");err!=nil{t.Fatal(err)}
    body:=map[string]any{
        "effect_id":attempt.ID,"actor":"operator-42","verdict":"absent",
        "reason":"checked upstream audit: no resource was created",
    }
    unauth:=jsonPost(t,api.URL+"/api/effect-decide","",body)
    unauth.Body.Close()
    if unauth.StatusCode!=http.StatusUnauthorized {t.Fatal("anonymous resolution allowed")}
    result:=decode(t,jsonPost(t,api.URL+"/api/effect-decide","test-secret",body))
    if result["status"]!="resolved_absent" {t.Fatalf("operator decision not durable: %#v",result)}
    evidence,ok:=result["resolution"].(map[string]any)
    if !ok || evidence["kind"]!="operator" || evidence["verifier"]!="operator-42"{
        t.Fatalf("operator action misclassified: %#v",result)
    }
    if _,err=svc.Store.BeginEffect("run-recovery","create","crm.create",map[string]any{"id":"C100"});
        err==nil {t.Fatal("resolved effect is replayable")}
    denied:=jsonPost(t,api.URL+"/api/effect-decide","test-secret",body)
    defer denied.Body.Close()
    if denied.StatusCode!=http.StatusBadRequest{
        raw,_:=io.ReadAll(denied.Body)
        t.Fatalf("duplicate operator decision: status=%d body=%s",denied.StatusCode,raw)
    }
    saved,err:=svc.Store.EffectGet(attempt.ID)
    if err!=nil || saved.Status!="resolved_absent" || saved.Resolution==nil {
        t.Fatalf("decision not persisted: %#v %v",saved,err)
    }
}
func TestManagementMCPReconciliationDecision(t *testing.T){
    svc,api,_:=fixture(t)
    effect,err:=svc.Store.BeginEffect("run-mcp","update","crm.update",map[string]any{"id":"C2"})
    if err!=nil{t.Fatal(err)}
    if err=svc.Store.CompleteEffect(effect.ID,"unknown","connection reset");err!=nil{t.Fatal(err)}
    result:=decode(t,latestCall(t,api.URL+"/mcp/admin","test-secret","tools/call",89,
        map[string]any{"name":"regate_effect_decide","arguments":map[string]any{
            "effect_id":effect.ID,"actor":"operator-7","verdict":"applied",
            "reason":"matched verified resource on upstream",
        }}))
    if result["error"]!=nil {t.Fatal(result)}
    data:=result["result"].(map[string]any)
    if data["isError"]==true {t.Fatalf("MCP operator tool failed: %#v",data)}
    current,err:=svc.Store.EffectGet(effect.ID)
    if err!=nil || current.Status!="resolved_applied"{
        t.Fatalf("MCP decision was not applied: %#v %v",current,err)
    }
    if current.Resolution.Kind!="operator" {t.Fatal("operator claim was misrepresented as verified read-back")}
}
