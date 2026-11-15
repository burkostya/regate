package server

import (
    "context"
    "strings"
    "testing"
)

func TestManagementMCPKeepsOpaqueDraftWithoutActivation(t *testing.T) {
    svc, _, _ := fixture(t)
    _, original := svc.Current()
    draft, err := svc.Store.DraftCreate()
    if err != nil { t.Fatal(err) }
    src := strings.Replace(draft.Source,
        `    tool      = "get_customer"`,
        `    tool      = "get_customer"
    vendor_extension = beta.config`,1)
    // Syntax-only drafts can hold unsupported/unevaluated configuration.
    _, err = svc.Admin(context.Background(), "regate_draft_patch",map[string]any{
        "draft_id":draft.ID, "source":src,
    })
    if err != nil { t.Fatalf("draft with opaque expression rejected: %v",err) }
    result, err := svc.Admin(context.Background(),"regate_attribute_patch",map[string]any{
        "draft_id":draft.ID, "path":"pipeline/customer_summary/node/fetch",
        "attribute":"tool", "expression":`"get_customer_v2"`,
    })
    if err != nil { t.Fatalf("cannot edit known field in an opaque draft: %v",err) }
    _ = result
    saved, err := svc.Store.DraftGet(draft.ID)
    if err != nil { t.Fatal(err) }
    if !strings.Contains(saved.Source, `vendor_extension = beta.config`) ||
        !strings.Contains(saved.Source,`tool      = "get_customer_v2"`) {
        t.Fatalf("draft lost opaque or edited HCL: %s",saved.Source)
    }
    validation, err := svc.Admin(context.Background(),"regate_validate",map[string]any{"draft_id":draft.ID})
    if err != nil { t.Fatal(err) }
    if validation.(map[string]any)["valid"] != false {
        t.Fatalf("unsupported HCL validated executable: %#v",validation)
    }
    if _, err := svc.Activate(draft.ID); err == nil {
        t.Fatal("unsupported HCL was activated")
    }
    _, after := svc.Current()
    if after.ID != original.ID {
        t.Fatalf("invalid draft changed active revision: %s to %s",original.ID,after.ID)
    }
}
