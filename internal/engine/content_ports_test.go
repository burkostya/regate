package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func mcpContentDrySource() string {
	return `upstream "stub" {
  url = "http://127.0.0.1:9099/mcp"
}
pipeline "reader" {
  entry = "fetch"
  node "fetch" {
    kind = "mcp.call"
    upstream = "stub"
    tool = "lookup"
    effect = "read"
    arguments = "input"
    mock = {display = "ordinary"}
    mock_content = [{type = "text",text = "hello"}]
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
    from = "fetch"
    to = "done"
  }
  edge {
    kind = "data"
    from = "fetch"
    from_port = "content"
    to = "done"
    to_port = "blocks"
  }
}
surface "content" {
  path = "/mcp/content"
  tool "lookup" {
    pipeline = "reader"
    read_only = true
  }
}
`
}

func TestMCPContentDryRunBindsTypedBlocks(t *testing.T) {
	cfg,err:=config.Decode(mcpContentDrySource())
	if err!=nil {t.Fatal(err)}
	out,err:=New(nil).Run(context.Background(),"rev",cfg,"content","lookup",map[string]any{},true)
	if err!=nil || out.Status!="success" || out.Value!="hello" {
		t.Fatalf("content port did not reach consumer: %#v %v",out,err)
	}
}

func TestMCPContentMissingOrMalformedFailClosed(t *testing.T) {
	base:=mcpContentDrySource()
	for _,tc:=range []struct{name,source,want string}{
		{"missing content",strings.Replace(base,`    mock_content = [{type = "text",text = "hello"}]`,"",1),"missing content MCP output"},
		{"wrong array",strings.Replace(base,`mock_content = [{type = "text",text = "hello"}]`,`mock_content = {type = "text",text = "hello"}`,1),"output port content failed schema validation"},
		{"wrong item type",strings.Replace(base,`mock_content = [{type = "text",text = "hello"}]`,`mock_content = [{type = "text",text = 8}]`,1),"output port content failed schema validation"},
	}{
		t.Run(tc.name,func(t *testing.T){
			cfg,err:=config.Decode(tc.source)
			if err!=nil {t.Fatal(err)}
			out,err:=New(nil).Run(context.Background(),"rev",cfg,"content","lookup",map[string]any{},true)
			if err==nil||!strings.Contains(err.Error(),tc.want)||out.Value!=nil {
				t.Fatalf("invalid content leaked through port: %#v %v",out,err)
			}
		})
	}
}

func TestMCPContentPortFromLiveEffectfulUpstream(t *testing.T) {
	upstream,calls:=testEffectUpstream(t,false)
	defer upstream.Close()
	source:=fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "writer" {
  entry = "create"
  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "input"
    output_ports = {
      content = {
        type = "array"
        items = {
          type = "object"
          required = ["type","text"]
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
    from = "create"
    to = "done"
  }
  edge {
    kind = "data"
    from = "create"
    from_port = "content"
    to = "done"
    to_port = "blocks"
  }
}
surface "content" {
  path = "/mcp/content"
  tool "create" {
    pipeline = "writer"
  }
}
`,upstream.URL)
	cfg,err:=config.Decode(source)
	if err!=nil {t.Fatal(err)}
	store:=effectTestStore(t)
	out,err:=New(store).Run(context.Background(),"rev",cfg,"content","create",map[string]any{"id":"C801"},false)
	if err!=nil || out.Status!="success" || out.Value!="created" || calls.Load()!=1 {
		t.Fatalf("live MCP content lost or replayed: %#v %v calls=%d",out,err,calls.Load())
	}
}
