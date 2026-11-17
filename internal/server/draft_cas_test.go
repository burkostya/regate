package server

import (
    "context"
    "errors"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
)

func TestManagementDraftCASPreventsStaleEditsAndUnreviewedActivation(t *testing.T) {
    svc,server,_:=fixture(t)
    ctx:=context.Background()
    original,err:=svc.Store.DraftCreate()
    if err!=nil {t.Fatal(err)}
    firstSource:=original.Source+"\n# independent HCL edit\n"
    first,err:=svc.Admin(ctx,"regate_draft_patch",map[string]any{
        "draft_id":original.ID,"source":firstSource,
        "expected_source_digest":original.SourceDigest,
    })
    if err!=nil {t.Fatal(err)}
    modified:=first.(storage.Draft)
    if modified.Source!=firstSource || modified.SourceDigest==original.SourceDigest {
        t.Fatalf("first write lost new HCL digest: %#v",modified)
    }

    for _,tc:=range []struct {
        name string
        args map[string]any
    }{
        {"stale HCL",map[string]any{"source":original.Source}},
        {"stale attribute",map[string]any{
            "path":"pipeline/customer_summary/node/fetch",
            "attribute":"tool","expression":`"get_customer_updated"`,
        }},
    } {
        name:="regate_draft_patch"
        if tc.name=="stale attribute" {name="regate_attribute_patch"}
        tc.args["draft_id"]=original.ID
        tc.args["expected_source_digest"]=original.SourceDigest
        if _,err:=svc.Admin(ctx,name,tc.args);!errors.Is(err,storage.ErrDraftSourceConflict) {
            t.Fatalf("%s overwrote a newer edit: %v",tc.name,err)
        }
    }
    staleHTTP:=jsonPost(t,server.URL+"/api/graph-patch","test-secret",map[string]any{
        "draft_id":original.ID,"pipeline":"customer_summary",
        "source":`pipeline "customer_summary" {}`,
        "expected_source_digest":original.SourceDigest,
    })
    staleHTTP.Body.Close()
    if staleHTTP.StatusCode!=400 {t.Fatalf("stale graph patch HTTP status %d",staleHTTP.StatusCode)}
    if _,err:=svc.ActivateIfDigest(original.ID,original.SourceDigest);!errors.Is(err,storage.ErrDraftSourceConflict) {
        t.Fatalf("activation ignored old reviewed digest: %v",err)
    }

    // A fresh client can still graph-patch the preserved opaque-safe HCL.
    current,err:=svc.Store.DraftGet(original.ID)
    if err!=nil {t.Fatal(err)}
    blocks,err:=config.ParseForEdit(current.Source)
    if err!=nil {t.Fatal(err)}
    replacement:=""
    for _,block:=range blocks {
        if block.Type=="pipeline" && len(block.Labels)==1 && block.Labels[0]=="customer_summary" {
            replacement=current.Source[block.Start:block.End]
            break
        }
    }
    if replacement=="" {t.Fatal("missing pipeline in fixture")}
    replacement=strings.Replace(replacement,`tool      = "get_customer"`,`tool      = "get_customer_v2"`,1)
    patched,err:=svc.Admin(ctx,"regate_graph_patch",map[string]any{
        "draft_id":original.ID,"pipeline":"customer_summary","source":replacement,
        "expected_source_digest":current.SourceDigest,
    })
    if err!=nil {t.Fatal(err)}
    reviewed:=patched.(storage.Draft)
    if reviewed.SourceDigest==current.SourceDigest || !strings.Contains(reviewed.Source,`"get_customer_v2"`) {
        t.Fatalf("fresh graph patch was lost: %#v",reviewed)
    }
    if _,err:=svc.ActivateIfDigest(reviewed.ID,current.SourceDigest);!errors.Is(err,storage.ErrDraftSourceConflict) {
        t.Fatalf("stale intermediate source was activated: %v",err)
    }
    activated,err:=svc.ActivateIfDigest(reviewed.ID,reviewed.SourceDigest)
    if err!=nil {t.Fatal(err)}
    persisted,source,err:=svc.Store.Active()
    if err!=nil || persisted.ID!=activated.ID || source!=reviewed.Source {
        t.Fatalf("prepared MCP catalog and committed revision differ: %#v %q %v",persisted,source,err)
    }
    cfg,rev:=svc.Current()
    if rev.ID!=activated.ID || cfg.Source!=reviewed.Source {
        t.Fatalf("service did not publish exactly the activated HCL: %#v %q",rev,cfg.Source)
    }
}

func TestManagementRejectsMalformedDraftDigest(t *testing.T) {
    svc,_,_:=fixture(t)
    draft,err:=svc.Store.DraftCreate()
    if err!=nil {t.Fatal(err)}
    for _,raw:=range []any{nil,42,"",strings.Repeat("z",64)} {
        _,err:=svc.Admin(context.Background(),"regate_draft_patch",map[string]any{
            "draft_id":draft.ID,"source":draft.Source,
            "expected_source_digest":raw,
        })
        if err==nil {t.Fatalf("invalid digest %#v was admitted",raw)}
    }
}
