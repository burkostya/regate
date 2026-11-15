package server

import (
    "context"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdminUpstreamRefreshIncludesFullMCPDiscovery(t *testing.T) {
    t.Setenv("REGATE_ADMIN_TOKEN","test-secret")
    upstream:=sdk.NewServer(&sdk.Implementation{Name:"catalog-provider",Version:"1"},
        &sdk.ServerOptions{PageSize:1})
    upstream.AddTool(&sdk.Tool{Name:"one",InputSchema:map[string]any{"type":"object"}},
        func(context.Context,*sdk.CallToolRequest)(*sdk.CallToolResult,error) {
            return &sdk.CallToolResult{Content:[]sdk.Content{&sdk.TextContent{Text:"ok"}}},nil
        })
    upstream.AddResource(&sdk.Resource{Name:"guide",URI:"fixture://guide",MIMEType:"text/plain"},
        func(context.Context,*sdk.ReadResourceRequest)(*sdk.ReadResourceResult,error) {
            return &sdk.ReadResourceResult{Contents:[]*sdk.ResourceContents{{URI:"fixture://guide",Text:"guide"}}},nil
        })
    upstream.AddResourceTemplate(&sdk.ResourceTemplate{Name:"dynamic",URITemplate:"fixture://items/{id}"},
        func(_ context.Context,req *sdk.ReadResourceRequest)(*sdk.ReadResourceResult,error) {
            return &sdk.ReadResourceResult{Contents:[]*sdk.ResourceContents{{URI:req.Params.URI,Text:"item"}}},nil
        })
    upstream.AddPrompt(&sdk.Prompt{Name:"analyze"},
        func(context.Context,*sdk.GetPromptRequest)(*sdk.GetPromptResult,error) {
            return &sdk.GetPromptResult{Messages:[]*sdk.PromptMessage{{Role:"user",Content:&sdk.TextContent{Text:"analyze"}}}},nil
        })
    endpoint:=httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request)*sdk.Server {return upstream},
        &sdk.StreamableHTTPOptions{Stateless:true,JSONResponse:true}))
    defer endpoint.Close()
    original,err:=os.ReadFile("../../examples/regate.hcl")
    if err!=nil {t.Fatal(err)}
    source:=strings.ReplaceAll(string(original),"http://127.0.0.1:9091/mcp",endpoint.URL)
    svc,err:=New(t.TempDir(),source)
    if err!=nil {t.Fatal(err)}
    ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
    defer cancel()
    value,err:=svc.Admin(ctx,"regate_upstream_refresh",map[string]any{"name":"crm"})
    if err!=nil {t.Fatal(err)}
    snapshot,ok:=value.(catalogEntry)
    if !ok {t.Fatalf("unexpected catalog result type %T",value)}
    if len(snapshot.Items)!=1 || len(snapshot.Resources)!=1 || len(snapshot.ResourceTemplates)!=1 || len(snapshot.Prompts)!=1 {
        t.Fatalf("discovery did not retain all upstream collections: %#v",snapshot)
    }
    if snapshot.Resources[0]["uri"]!="fixture://guide" || snapshot.Prompts[0]["name"]!="analyze" {
        t.Fatalf("unexpected catalog entries: %#v",snapshot)
    }
    cached,err:=svc.Discover(ctx,"crm",false)
    if err!=nil {t.Fatal(err)}
    if len(cached.Prompts)!=1 {t.Fatalf("cached discovery lost prompts: %#v",cached)}
}
