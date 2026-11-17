package config

import (
	"strings"
	"testing"
)

func mcpContentPortFixture() string {
	return `upstream "stub" {
  url = "http://127.0.0.1:9099/mcp"
}
pipeline "read" {
  entry = "lookup"
  node "lookup" {
    kind = "mcp.call"
    upstream = "stub"
    tool = "lookup"
    effect = "read"
    arguments = "input"
    mock = {label = "ordinary"}
    mock_content = [{type = "text", text = "Hello"}]
    output_ports = {
      content = {
        type = "array"
        items = {
          type = "object"
          required = ["type", "text"]
          properties = {
            type = {type = "string"}
            text = {type = "string"}
          }
        }
      }
    }
  }
  node "done" {
    kind = "output"
    input_ports = {
      blocks = {
        type = "array"
        items = {type = "object"}
      }
    }
    value = "ports.blocks[0].text"
  }
  edge {
    from = "lookup"
    to = "done"
  }
  edge {
    kind = "data"
    from = "lookup"
    from_port = "content"
    to = "done"
    to_port = "blocks"
  }
}
surface "content" {
  path = "/mcp/content"
  tool "read" {
    pipeline = "read"
    read_only = true
  }
}
`
}

func TestMCPContentPortCompilesAsTypedDataEdge(t *testing.T) {
	cfg,err:=Decode(mcpContentPortFixture())
	if err!=nil {t.Fatal(err)}
	port:=cfg.Plans["read"].Nodes["lookup"].OutputPorts["content"]
	if port==nil || len(port.Types())!=1 || port.Types()[0]!="array" {
		t.Fatal("MCP content contract was not compiled as array")
	}
	if edge:=cfg.Pipelines["read"].Edges[1];edge.FromPort!="content" || edge.ToPort!="blocks" {
		t.Fatalf("content edge lost: %#v",edge)
	}
}

func TestInvalidMCPContentDeclarationsFailCompilation(t *testing.T) {
	base:=mcpContentPortFixture()
	tests:=[]struct{name,source,want string}{
		{"without declaration",strings.Replace(base,`    output_ports = {
      content = {
        type = "array"
        items = {
          type = "object"
          required = ["type", "text"]
          properties = {
            type = {type = "string"}
            text = {type = "string"}
          }
        }
      }
    }`,"",1),"requires a declared MCP content output port"},
		{"wrong content type",strings.Replace(base,`content = {
        type = "array"`,`content = {
        type = "string"`,1),"explicit array JSON Schema required"},
		{"unknown content type",strings.Replace(base,`content = {
        type = "array"`,`content = {
        type = "not_a_type"`,1),"output port content"},
		{"incompatible consumer",strings.Replace(base,`blocks = {
        type = "array"`,`blocks = {
        type = "integer"`,1),"incompatible port types"},
		{"unrecognized port",strings.Replace(base,`from_port = "content"`,`from_port = "headers"`,1),"invalid data edge"},
	}
	for _,tc:=range tests {
		t.Run(tc.name,func(t *testing.T){
			if tc.source==base {t.Fatal("fixture replacement did not match")}
			if _,err:=Decode(tc.source);err==nil||!strings.Contains(err.Error(),tc.want) {
				t.Fatalf("invalid MCP content contract accepted: %v",err)
			}
		})
	}
}
