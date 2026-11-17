package config

import (
    "strings"
    "testing"
)

// The graph editor must not erase fields it cannot understand. Unsupported
// executable HCL is kept only in an inactive draft and remains fail-closed.
func TestOpaqueDraftStructuralGraphRoundTrip(t *testing.T) {
    src := example(t)
    src = strings.Replace(src, `    tool      = "get_customer"`, `    tool      = "get_customer"
    vendor_expression = upstream.features.flags["beta"]`, 1)
    src = strings.Replace(src, `  node "summarize" {`, `  future_node "provider" {
    feature = registry_experimental()
  }

  node "summarize" {`, 1)
    src += "\nfuture_top \"opaque\" { expression = var.unavailable }\n"
    if _, err := Decode(src); err == nil {
        t.Fatal("unsupported opaque HCL activated")
    }
    blocks, err := ParseForEdit(src)
    if err != nil { t.Fatalf("syntax-only parse refused valid HCL: %v", err) }
    var original string
    for _, b := range blocks {
        if b.Type == "pipeline" && len(b.Labels) == 1 && b.Labels[0] == "customer_summary" {
            original = src[b.Start:b.End]
        }
    }
    if original == "" { t.Fatal("pipeline not parsed") }
    // The UI emits only the executable blocks and known fields it supports.
    // Absent opaque content must survive, rather than being interpreted as a
    // user-requested destructive deletion.
    desired := strings.Replace(original, `tool      = "get_customer"`, `tool      = "get_customer_v2"`, 1)
    desired = strings.Replace(desired, "    vendor_expression = upstream.features.flags[\"beta\"]\n", "", 1)
    desired = strings.Replace(desired, `  future_node "provider" {
    feature = registry_experimental()
  }

`, "", 1)
    next, err := ReplaceBlock(src, "pipeline", "customer_summary", desired)
    if err != nil { t.Fatal(err) }
    for _, fragment := range []string{
        `vendor_expression = upstream.features.flags["beta"]`,
        `future_node "provider"`,
        `feature = registry_experimental()`,
        `future_top "opaque" { expression = var.unavailable }`,
    } {
        if !strings.Contains(next, fragment) { t.Fatalf("lost opaque content %q", fragment) }
    }
    if !strings.Contains(next, `tool      = "get_customer_v2"`) {
        t.Fatal("known attribute was not updated")
    }
    if _, err := Decode(next); err == nil {
        t.Fatal("draft became executable with unknown fragments")
    }
    if _, err := ParseForEdit(next); err != nil {
        t.Fatalf("edited source became syntactically invalid: %v", err)
    }
    again, err := ReplaceBlock(next, "pipeline", "customer_summary", desired)
    if err != nil { t.Fatal(err) }
    if again != next { t.Fatal("repeat patch changed opaque source bytes") }
}

func TestOpaqueDraftAttributeEditsAndFailClosedActivation(t *testing.T) {
    src := example(t) + "\nother \"extension\" { endpoint = arbitrary.symbol }\n"
    path := []BlockSelector{{Type:"pipeline", Label:"customer_summary"}, {Type:"node", Label:"fetch"}}
    updated, err := SetAttribute(src, path, "tool", `"get_customer_safe"`)
    if err != nil { t.Fatal(err) }
    if !strings.HasSuffix(updated, `other "extension" { endpoint = arbitrary.symbol }`+"\n") {
        t.Fatal("source outside edit changed")
    }
    updated, err = AddAttribute(updated, path, "input_schema", `{type="object",properties={customer_id={type="string"}}}`)
    if err != nil { t.Fatal(err) }
    if _, err := Decode(updated); err == nil { t.Fatal("unknown draft activated") }
    if _, err := SetAttribute(updated, path, "vendor", `true`); err == nil {
        t.Fatal("graph patch should not create an unsupported attribute")
    }
    if _, err := AddAttribute(updated, path, "opaque_future", `true`); err == nil {
        t.Fatal("graph patch inserted opaque attribute")
    }
    // A known-good configuration still rejects invalid graph edits.
    if _, err := SetAttribute(example(t), path, "upstream", `"missing"`); err == nil {
        t.Fatal("valid active source was changed into invalid executable HCL")
    }
}

func TestOpaqueDraftChangesCannotBeSilentlyRewritten(t *testing.T) {
    src := strings.Replace(example(t), `    tool      = "get_customer"`, `    tool      = "get_customer"
    vendor = custom.data`, 1)
    blocks, err := ParseForEdit(src)
    if err != nil { t.Fatal(err) }
    var original string
    for _, b := range blocks { if b.Type=="pipeline" && b.Labels[0]=="customer_summary" { original=src[b.Start:b.End] } }
    if _, err := ReplaceBlock(src,"pipeline","customer_summary",
        strings.Replace(original, `vendor = custom.data`, `vendor = custom.other`,1)); err == nil {
        t.Fatal("silent opaque mutation through structural patch")
    }
    if _, err := ReplaceBlock(src,"pipeline","customer_summary",
        strings.Replace(original, `  node "summarize" {`, "  future_node \"new\" { x = 2 }\n  node \"summarize\" {",1)); err == nil {
        t.Fatal("graph patch silently inserted unknown block")
    }
    if _, err := ParseForEdit(`pipeline "bad" { kind = [ }`); err == nil {
        t.Fatal("syntax-invalid draft accepted")
    }
}

func TestAttributePatchCanRepairUnknownKind(t *testing.T) {
    original := example(t)
    src := strings.Replace(original, `kind      = "mcp.call"`, `kind      = custom.unknown_kind`,1)
    if src == original { t.Fatal("test fixture kind missing") }
    if _, err := Decode(src); err == nil { t.Fatal("dynamic kind should be rejected on activation") }
    path := []BlockSelector{{Type:"pipeline",Label:"customer_summary"}, {Type:"node",Label:"fetch"}}
    repaired, err := SetAttribute(src,path,"kind",`"mcp.call"`)
    if err != nil { t.Fatalf("could not repair unknown node kind: %v",err) }
    if repaired != original { t.Fatalf("known kind repair changed unrelated source") }
    if _, err := Decode(repaired); err != nil { t.Fatal(err) }
}


func TestGraphPatchAddsTypedDataPortWhilePreservingControlAndSource(t *testing.T) {
    complete:=dataPortHCL()
    declared:=`    input_ports = {
      text = {type = "string"}
    }`
    dataEdge:=`  edge {
    kind = "data"
    from = "produce"
    to = "finish"
    from_port = "value"
    to_port = "text"
  }`
    if !strings.Contains(complete,declared) || !strings.Contains(complete,dataEdge) {
        t.Fatal("typed port fixture changed")
    }
    // Both revisions are executable. A graph patch introduces a previously
    // absent input_ports contract AND its data-edge binding in one operation.
    original:=strings.Replace(complete,`value = "ports.text"`,`value = "literal:ok"`,1)
    original=strings.Replace(original,declared,"",1)
    original=strings.Replace(original,dataEdge,"",1)
    if _,err:=Decode(original);err!=nil {t.Fatalf("invalid original: %v",err)}
    desired:=strings.Replace(complete,`value = "ports.text"`,`value = "literal:ok"`,1)
    blocks,err:=ParseForEdit(desired)
    if err!=nil {t.Fatal(err)}
    replacement:=""
    for _,block:=range blocks {
        if block.Type=="pipeline" && len(block.Labels)==1 && block.Labels[0]=="ports" {
            replacement=desired[block.Start:block.End]
        }
    }
    if replacement=="" {t.Fatal("missing pipeline")}
    updated,err:=ReplaceBlock(original,"pipeline","ports",replacement)
    if err!=nil {t.Fatalf("cannot add typed input_ports through graph patch: %v",err)}
    if _,err=Decode(updated);err!=nil {t.Fatalf("patched graph invalid: %v",err)}
    if !strings.Contains(updated,declared) || !strings.Contains(updated,dataEdge) {
        t.Fatalf("lost typed port or data edge: %s",updated)
    }
    if !strings.Contains(updated,`    from = "produce"
    to = "finish"
  }`) {
        t.Fatal("graph patch removed separate control dependency")
    }
    if !strings.HasSuffix(updated, original[strings.Index(original,`surface "ports"`):]) {
        t.Fatal("graph patch rewrote unrelated surface HCL")
    }
    again,err:=ReplaceBlock(updated,"pipeline","ports",replacement)
    if err!=nil || again!=updated {t.Fatalf("patch is not idempotent: %v",err)}
}

func TestKnownOutputPortsCanBeAddedByGraphEditor(t *testing.T) {
    src:=example(t)
    path:=[]BlockSelector{{Type:"pipeline",Label:"customer_summary"},{Type:"node",Label:"fetch"}}
    updated,err:=AddAttribute(src,path,"output_ports",`{structured={type="object"}}`)
    if err!=nil {t.Fatalf("output_ports should be an editable compiler field: %v",err)}
    if _,err=Decode(updated);err!=nil {t.Fatalf("output port contract broke compilation: %v",err)}
}
