package server

import (
    "context"
    "errors"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/mcp"
    "github.com/burkostya/regate/internal/storage"
)

// Independent third-party write-contract acceptance, never part of default
// offline tests. Sondahub documents that the external writes are simulated,
// session-token scoped and never persisted as global data. We deliberately
// use only fixed synthetic records, and no credentials or real identities.
func TestPublicSimulatedMCPWriteAndError(t *testing.T) {
    if os.Getenv("REGATE_TEST_PUBLIC_MCP_WRITES")!="1" {
        t.Skip("set REGATE_TEST_PUBLIC_MCP_WRITES=1 for live third-party simulated write acceptance")
    }
    t.Setenv("REGATE_ALLOWED_HOSTS","api.sondahub.com")
    t.Setenv("REGATE_ADMIN_TOKEN","synthetic-third-party-write-local-admin-only")

    fixture,err:=os.ReadFile("../../examples/public-simulated-write-mcp.hcl")
    if err!=nil {t.Fatal(err)}
    svc,err:=New(t.TempDir(),string(fixture))
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    gatewayHTTP:=httptest.NewServer(svc.Handler())
    defer gatewayHTTP.Close()

    ctx,cancel:=context.WithTimeout(context.Background(),110*time.Second)
    defer cancel()
    gateway:=mcp.NewConn(gatewayHTTP.URL+"/mcp/sandbox","",45*time.Second)
    catalog,err:=gateway.ListTools(ctx)
    if err!=nil {t.Fatalf("synthetic write fixture discovery: %v",err)}
    seen:=map[string]bool{}
    for _,tool:=range catalog {if name,ok:=tool["name"].(string);ok {seen[name]=true}}
    if len(catalog)!=2 || !seen["create_fixture"] || !seen["reject_fixture"] {
        t.Fatalf("simulated write fixture leaked unexpected MCP tools: %v",seen)
    }

    accepted,err:=gateway.CallTool(ctx,"create_fixture",map[string]any{})
    if err!=nil || accepted==nil || accepted["isError"]==true {
        t.Fatalf("externally simulated write did not succeed: %v isError=%t",err,accepted!=nil && accepted["isError"]==true)
    }
    structured,ok:=accepted["structuredContent"].(map[string]any)
    if !ok || structured["simulation_note_present"]!=true {
        t.Fatalf("provider's documented simulated-write result marker absent: %v",ok)
    }
    effects:=svc.Store.Effects()
    if len(effects)!=1 || effects[0].Status!="confirmed" ||
        effects[0].Operation!="sondahub.create_record" ||
        effects[0].TargetDigest!=storage.TargetDigest("mcp","https://api.sondahub.com/mcp") ||
        effects[0].IntentDigest=="" {
        t.Fatalf("successful third-party simulated write journal incorrect: %#v",effects)
    }

    // This vendor deliberately returns isError=true for invalid writes.
    // Such a response cannot prove that a real remote system did not commit.
    // Regate must conservatively mark it UNKNOWN, never confirmed.
    rejected,rejectErr:=gateway.CallTool(ctx,"reject_fixture",map[string]any{})
    if rejectErr==nil && (rejected==nil || rejected["isError"]!=true) {
        t.Fatalf("schema-invalid provider write unexpectedly succeeded: %#v",rejected)
    }
    effects=svc.Store.Effects()
    if len(effects)!=2 {t.Fatalf("expected one journal admission for each real external call: %d",len(effects))}
    statuses:=map[string]bool{}
    var failed storage.Effect
    for _,e:=range effects {
        statuses[e.Status]=true
        if e.Status=="unknown" {failed=e}
    }
    if !statuses["confirmed"] || !statuses["unknown"] || failed.ID=="" {
        t.Fatalf("MCP application error did not become unknown: %#v",effects)
    }
    if failed.RunID==effects[0].RunID && failed.RunID==effects[1].RunID {
        t.Fatal("independent third-party writes share a run identity")
    }
    // The exact immutable invalid intent cannot be admitted for another
    // network dispatch in that run, even if its remote status is uncertain.
    intent:=map[string]any{"api":"store","collection":"categories",
        "record":map[string]any{"name":"Regate intentionally invalid synthetic category"}}
    if _,err:=svc.Store.BeginEffectBound(failed.RunID,failed.Node,failed.Operation,
        failed.TargetDigest,intent);!errors.Is(err,storage.ErrEffectAlreadyAttempted) {
        t.Fatalf("unknown third-party write became replayable: %v",err)
    }

    // The client cannot change any of the pinned synthetic write arguments.
    denied,deniedErr:=gateway.CallTool(ctx,"create_fixture",
        map[string]any{"api":"bank","collection":"accounts"})
    if deniedErr==nil && (denied==nil || denied["isError"]!=true) {
        t.Fatal("unapproved external write arguments accepted")
    }
    if len(svc.Store.Effects())!=2 {
        t.Fatal("rejected caller input was forwarded to the remote simulated writer")
    }
    if !strings.Contains(string(fixture),"literal:store") {
        t.Fatal("fixture lost its pinned synthetic external-write arguments")
    }
    t.Log("independent provider: simulated write confirmed; isError write unknown; no re-admission or injected arguments")
}
