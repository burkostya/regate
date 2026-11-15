package server

import (
    "context"
    "errors"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/engine"
)

func TestRestrictedOAuthPipelineTrustFailsClosedBeforeEffects(t *testing.T) {
    svc,_,world,_:=oauthFixture(t)
    current,rev:=svc.Current()
    candidate,err:=config.Decode(current.Source)
    if err!=nil {t.Fatal(err)}
    crm:=candidate.Upstreams["crm"]
    crm.TrustLevel="" // Unknown destinations are not implicitly trusted.
    candidate.Upstreams["crm"]=crm
    _,err=svc.Engine.Run(context.Background(),rev.ID,candidate,"support","get_customer_direct",
        map[string]any{"customer_id":"C1"},false)
    if !errors.Is(err,engine.ErrUntrustedDestination) {
        t.Fatalf("untrusted MCP upstream was called or misclassified: %v",err)
    }
    if len(svc.Store.Effects())!=0 {t.Fatal("denied trust boundary journaled a non-existent external effect")}
    world.mu.Lock()
    if len(world.calls)!=0 {t.Fatalf("restricted CRM call reached the network: %v",world.calls)}
    world.mu.Unlock()

    candidate,err=config.Decode(current.Source)
    if err!=nil {t.Fatal(err)}
    hook:=candidate.Hooks["summarizer"]
    hook.TrustLevel="" // Even a future conditional branch must be vetted.
    candidate.Hooks["summarizer"]=hook
    _,err=svc.Engine.Run(context.Background(),rev.ID,candidate,"support","summarize_customer",
        map[string]any{"customer_id":"C123"},false)
    if !errors.Is(err,engine.ErrUntrustedDestination) {
        t.Fatalf("untrusted agent hook bypassed preflight: %v",err)
    }
    world.mu.Lock()
    defer world.mu.Unlock()
    if len(world.calls)!=0 {t.Fatalf("preflight touched upstream despite forbidden agent sink: %v",world.calls)}
}

func TestTrustedOAuthPipelinePreservesDecisionAndRedaction(t *testing.T) {
    _,srv,world,private:=oauthFixture(t)
    token:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read crm:read")
    data:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"tools/call",150,map[string]any{
        "name":"summarize_customer","arguments":map[string]any{"customer_id":"C123"},
    }))
    result:=data["result"].(map[string]any)
    if result["isError"]==true || data["error"]!=nil {t.Fatalf("trusted restricted pipeline failed: %v",data)}
    world.mu.Lock()
    defer world.mu.Unlock()
    if world.agentInput==nil || world.agentInput["email"]!="[REDACTED]" {
        t.Fatalf("agent did not receive mandatory masked data: %#v",world.agentInput)
    }
}

func TestExplicitTrustDoesNotBypassIndependentScopes(t *testing.T) {
    _,srv,world,private:=oauthFixture(t)
    token:=signedTestJWT(t,private,"https://issuer.test","https://regate.test/mcp/support","surface:read")
    denied:=decode(t,latestCall(t,srv.URL+"/mcp/support",token,"tools/call",151,map[string]any{
        "name":"summarize_customer","arguments":map[string]any{"customer_id":"C123"},
    }))
    if denied["result"].(map[string]any)["isError"]!=true {t.Fatalf("trusted destination bypassed user authorization: %#v",denied)}
    world.mu.Lock()
    defer world.mu.Unlock()
    if len(world.calls)!=0 {t.Fatalf("scope-denied call touched upstream: %v",world.calls)}
}
