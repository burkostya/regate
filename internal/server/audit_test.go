package server

import (
    "context"
    "encoding/json"
    "io"
    "net/http"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestManagementMutationsCreateRedactedAuditedBeforeAndAfterRecords(t *testing.T) {
    svc,srv,_:=fixture(t)
    created,err:=svc.Admin(context.Background(),"regate_draft_create",nil)
    if err!=nil {t.Fatal(err)}
    draft,ok:=created.(storage.Draft)
    if !ok {t.Fatalf("unexpected draft result %T",created)}
    secret:="SECRETS_SHOULD_NOT_APPEAR_IN_AUDIT"
    _,err=svc.Admin(context.Background(),"regate_draft_patch",map[string]any{
        "draft_id":draft.ID,"source":draft.Source+"\n# "+secret+"\n",
    })
    if err!=nil {t.Fatal(err)}
    // Failed sensitive operations have a durable start and a failure event,
    // without persisting the reason, HCL source or arbitrary output.
    _,err=svc.Admin(context.Background(),"regate_rollback",map[string]any{"revision":"invalid"})
    if err==nil {t.Fatal("expected rollback error")}
    records,err:=svc.Store.AuditTail(200)
    if err!=nil || len(records)!=6 {t.Fatalf("management events missing: %d %v",len(records),err)}
    if records[0].Action!="regate_rollback" || records[0].Outcome!="failed" ||
        records[1].Outcome!="started" {t.Fatalf("failed mutation auditing wrong: %#v",records[:2])}
    response,err:=http.Get(srv.URL+"/api/audit")
    if err!=nil {t.Fatal(err)}
    response.Body.Close()
    if response.StatusCode!=401 {t.Fatalf("anonymous audit accepted: %d",response.StatusCode)}
    req,_:=http.NewRequest("GET",srv.URL+"/api/audit",nil)
    req.Header.Set("Authorization","Bearer test-secret")
    response,err=http.DefaultClient.Do(req)
    if err!=nil {t.Fatal(err)}
    defer response.Body.Close()
    if response.StatusCode!=200 {t.Fatalf("admin audit error: %d",response.StatusCode)}
    body,err:=io.ReadAll(response.Body)
    if err!=nil {t.Fatal(err)}
    if strings.Contains(string(body),secret) || strings.Contains(string(body),"test-secret") {
        t.Fatal("audit endpoint leaked HCL content or token")
    }
    var decoded []storage.AuditRecord
    if err=json.Unmarshal(body,&decoded);err!=nil || len(decoded)!=6 {
        t.Fatalf("unexpected audit API: %s %v",body,err)
    }
    mcpAudit,err:=svc.Admin(context.Background(),"regate_audit",nil)
    if err!=nil {t.Fatal(err)}
    if len(mcpAudit.([]storage.AuditRecord))!=6 {t.Fatal("admin MCP audit projection differs from REST")}
}
