package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func errorFlagUpstream(t *testing.T) (*httptest.Server,*atomic.Int64,*atomic.Int64) {
	t.Helper()
	failed:=new(atomic.Int64)
	written:=new(atomic.Int64)
	server:=sdk.NewServer(&sdk.Implementation{Name:"application-error-test",Version:"1"},nil)
	server.AddTool(&sdk.Tool{Name:"fail",InputSchema:map[string]any{"type":"object"}},
		func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
			failed.Add(1)
			return &sdk.CallToolResult{
				IsError:true,
				Content:[]sdk.Content{&sdk.TextContent{Text:"validation failed"}},
				StructuredContent:map[string]any{"decision":"allow","ok":true},
			},nil
		})
	server.AddTool(&sdk.Tool{Name:"write",InputSchema:map[string]any{"type":"object"}},
		func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
			written.Add(1)
			return &sdk.CallToolResult{
				Content:[]sdk.Content{&sdk.TextContent{Text:"wrote"}},
			},nil
		})
	return httptest.NewServer(sdk.NewStreamableHTTPHandler(
		func(*http.Request)*sdk.Server{return server},
		&sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true})),failed,written
}

func errorFlagPipeline(url string) string {
	return fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "unsafe_chain" {
  entry = "read"
  node "read" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "fail"
    effect = "read"
    arguments = "input"
  }
  node "write" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "write"
    effect = "write"
    arguments = "nodes.read.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.write.value"
  }
  edge {
    from = "read"
    to = "write"
  }
  edge {
    from = "write"
    to = "done"
  }
}
surface "test" {
  path = "/mcp/test"
  tool "run" {
    pipeline = "unsafe_chain"
  }
}
`,url)
}

func TestMCPIsErrorPreventsDownstreamEffectsAndOutput(t *testing.T) {
	upstream,failed,written:=errorFlagUpstream(t)
	defer upstream.Close()
	cfg,err:=config.Decode(errorFlagPipeline(upstream.URL))
	if err!=nil {t.Fatal(err)}
	store:=effectTestStore(t)
	out,err:=New(store).Run(context.Background(),"rev",cfg,"test","run",
		map[string]any{"id":"C901"},false)
	if err==nil || !strings.Contains(err.Error(),"isError") ||
		out.Status=="success" || out.Value!=nil || failed.Load()!=1 || written.Load()!=0 {
		t.Fatalf("reported MCP error activated downstream effect: %#v %v errors=%d writes=%d",
			out,err,failed.Load(),written.Load())
	}
}

func TestMCPIsErrorHookCannotAuthorizeOrTransform(t *testing.T) {
	upstream,failed,written:=errorFlagUpstream(t)
	defer upstream.Close()
	store:=effectTestStore(t)
	r:=New(store)
	cfg:=&config.Config{Upstreams:map[string]config.Upstream{
		"crm":{Name:"crm",URL:upstream.URL,TimeoutMS:5000},
	}}
	for _,contract:=range []string{"decision","transform"} {
		ctx:=withEffectStep(withEffectScope(context.Background(),storage.ID()),"hook:"+contract)
		h:=config.Hook{Name:"guard",Transport:"mcp",Upstream:"crm",Tool:"fail",Contract:contract}
		got,err:=r.runHook(ctx,h,cfg,map[string]any{"id":"C902"},false)
		if err==nil || !strings.Contains(err.Error(),"isError") || got!=nil {
			t.Fatalf("isError passed %s hook: %#v %v",contract,got,err)
		}
	}
	if failed.Load()!=2 || written.Load()!=0 {
		t.Fatalf("incorrect hook dispatch count errors=%d writes=%d",failed.Load(),written.Load())
	}
}

func TestDirectMCPProxyPreservesToolErrorEnvelope(t *testing.T) {
	upstream,failed,_:=errorFlagUpstream(t)
	defer upstream.Close()
	r:=New(nil)
	u:=config.Upstream{Name:"crm",URL:upstream.URL,TimeoutMS:5000}
	cfg:=&config.Config{Upstreams:map[string]config.Upstream{"crm":u}}
	raw,err:=r.mcpCall(context.Background(),cfg,u,"fail",map[string]any{},
		"proxy","read","read")
	if err!=nil || raw["isError"]!=true || failed.Load()!=1 {
		t.Fatalf("transparent proxy changed standard MCP isError: %#v %v",raw,err)
	}
}

func TestMCPApplicationErrorsAreNotReplayPermissions(t *testing.T) {
	upstream,failed,_:=errorFlagUpstream(t)
	defer upstream.Close()
	store:=effectTestStore(t)
	r:=New(store)
	u:=config.Upstream{Name:"crm",URL:upstream.URL,TimeoutMS:5000}
	cfg:=&config.Config{Upstreams:map[string]config.Upstream{"crm":u}}
	ctx:=context.Background()
	raw,err:=r.mcpCall(ctx,cfg,u,"fail",map[string]any{"id":"C903"},"error-run","write","write")
	if err!=nil || raw["isError"]!=true {t.Fatalf("unexpected error envelope: %#v %v",raw,err)}
	if err:=validateMCPToolOutcome(raw);err==nil {t.Fatal("isError accepted by caller")}
	_,err=r.mcpCall(ctx,cfg,u,"fail",map[string]any{"id":"C903"},"error-run","write","write")
	if !errors.Is(err,storage.ErrEffectAlreadyAttempted) || failed.Load()!=1 {
		t.Fatalf("errored write blindly replayed: %v calls=%d",err,failed.Load())
	}
}

func TestMCPToolErrorFlagMustBeWellFormed(t *testing.T) {
	for _,raw:=range []map[string]any{nil,{"isError":"true"},{"isError":1},{"isError":true}} {
		if err:=validateMCPToolOutcome(raw);err==nil {
			t.Fatalf("malformed/errored MCP result accepted: %#v",raw)
		}
	}
	for _,raw:=range []map[string]any{{},{"isError":false}} {
		if err:=validateMCPToolOutcome(raw);err!=nil {
			t.Fatalf("valid MCP result rejected: %#v %v",raw,err)
		}
	}
}
