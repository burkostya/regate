package config

import (
    "strings"
    "testing"
)

func mappedSubpipelineSource() string {
    return `pipeline "item" {
  entry = "start"
  node "start" {
    kind = "input"
    input_schema = {type = "object"}
  }
  node "done" {
    kind = "output"
    value = "nodes.start.value"
    output_schema = {type = "object"}
  }
  edge {
    from = "start"
    to = "done"
  }
}
pipeline "batch" {
  entry = "each"
  node "each" {
    kind = "map"
    items = "input.items"
    pipeline = "item"
    max_items = 2
    input_schema = {type = "object"}
    output_schema = {type = "array", items = {type = "object"}}
  }
  node "done" {
    kind = "output"
    value = "nodes.each.value"
  }
  edge {
    from = "each"
    to = "done"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "run" {
    pipeline = "batch"
    read_only = true
  }
}
`
}

func TestMappedPipelineHasTypedItemAndResultContracts(t *testing.T) {
    cfg,err:=Decode(mappedSubpipelineSource())
    if err!=nil {t.Fatal(err)}
    item:=cfg.Plans["batch"].Nodes["each"]
    if item.Expressions["items"]==nil || item.Expressions["mapping"]!=nil ||
        item.InputSchema==nil || item.OutputSchema==nil {
        t.Fatalf("mapped child contracts missing: %#v",item)
    }
}

func TestMappedPipelineInvalidDeclarationsFailClosed(t *testing.T) {
    base:=mappedSubpipelineSource()
    cases:=[]struct {name,source,expected string}{
        {"both mapping and child",strings.Replace(base,`    pipeline = "item"`,
            `    pipeline = "item"
    mapping = "input"`,1),"exactly one of mapping or pipeline"},
        {"neither mapping nor child",strings.Replace(base,`    pipeline = "item"`,"",1),
            "exactly one of mapping or pipeline"},
        {"effects without callee",strings.Replace(base,`    pipeline = "item"`,
            `    mapping = "input"
    allow_effects = true`,1),"allow_effects requires pipeline"},
        {"unknown callee",strings.Replace(base,`pipeline = "item"`,`pipeline = "missing"`,1),
            "unknown called pipeline"},
        {"nonobject input",strings.Replace(base,`input_schema = {type = "object"}
    output_schema = {type = "array"`,
            `input_schema = {type = "string"}
    output_schema = {type = "array"`,1),"requires object input_schema"},
        {"nonarray output",strings.Replace(base,`output_schema = {type = "array", items = {type = "object"}}`,
            `output_schema = {type = "object"}`,1),"requires array output_schema"},
        {"missing result items",strings.Replace(base,`output_schema = {type = "array", items = {type = "object"}}`,
            `output_schema = {type = "array"}`,1),"needs items JSON Schema"},
        {"incompatible result items",strings.Replace(base,`output_schema = {type = "array", items = {type = "object"}}`,
            `output_schema = {type = "array", items = {type = "string"}}`,1),"incompatible port types"},
        {"multiple child outputs",strings.Replace(base,`  edge {
    from = "start"
    to = "done"
  }`,`  node "other" {
    kind = "output"
    value = "nodes.start.value"
    output_schema = {type = "object"}
  }
  edge {
    from = "start"
    to = "other"
  }
  edge {
    from = "start"
    to = "done"
  }`,1),"multi-output map requires closed object items schema"},
    }
    for _,tc:=range cases {
        t.Run(tc.name,func(t *testing.T){
            _,err:=Decode(tc.source)
            if err==nil || !strings.Contains(err.Error(),tc.expected) {
                t.Fatalf("wanted %q, got %v",tc.expected,err)
            }
        })
    }
}
