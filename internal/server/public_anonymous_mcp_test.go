package server

import (
    "context"
    "encoding/json"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/burkostya/regate/internal/mcp"
)

// TestPublicAnonymousMCPProviders is an explicitly enabled, real-network
// integration test against two independently operated, read-only, public MCP
// services. Unlike the local alpha/beta stand, these are not our servers.
//
// It is opt-in because independent public services can be rate limited,
// unavailable, or change their catalog independently of Regate. Never send
// private inputs or execute write-capable tools through this smoke test.
func TestPublicAnonymousMCPProviders(t *testing.T) {
    if os.Getenv("REGATE_TEST_PUBLIC_MCP")!="1" {
        t.Skip("set REGATE_TEST_PUBLIC_MCP=1 to contact live external MCP servers")
    }
    t.Setenv("REGATE_ALLOWED_HOSTS","learn.microsoft.com,mcp.deepwiki.com")
    t.Setenv("REGATE_ADMIN_TOKEN","public-mcp-smoke-local-admin-only")

    fixture,err:=os.ReadFile("../../examples/public-anonymous-mcp.hcl")
    if err!=nil {t.Fatal(err)}
    svc,err:=New(t.TempDir(),string(fixture))
    if err!=nil {t.Fatal(err)}
    defer svc.Store.Close()
    proxy:=httptest.NewServer(svc.Handler())
    defer proxy.Close()

    providers:=[]struct{
        name string
        endpoint string
        tool string
        published string
        input map[string]any
    }{
        {
            name:"mslearn",
            endpoint:"https://learn.microsoft.com/api/mcp",
            tool:"microsoft_docs_search",
            published:"search_docs",
            input:map[string]any{"query":"What is the Microsoft Learn MCP server?"},
        },
        {
            name:"deepwiki",
            endpoint:"https://mcp.deepwiki.com/mcp",
            tool:"read_wiki_structure",
            published:"wiki_structure",
            input:map[string]any{"repoName":"modelcontextprotocol/servers"},
        },
    }
    ctx,cancel:=context.WithTimeout(context.Background(),100*time.Second)
    defer cancel()
    for _,provider:=range providers {
        t.Run(provider.name,func(t *testing.T) {
            // Anonymous SDK handshake + tools/list against the actual vendor.
            upstream:=mcp.NewConn(provider.endpoint,"",30*time.Second)
            catalog,err:=upstream.ListTools(ctx)
            if err!=nil {t.Fatalf("anonymous external MCP discovery: %v",err)}
            found:=false
            for _,entry:=range catalog {
                if entry["name"]==provider.tool {found=true}
            }
            if !found {t.Fatalf("public provider no longer advertises %q (tools count=%d)",provider.tool,len(catalog))}
            if len(catalog)==0 {t.Fatal("external MCP tool catalog empty")}

            // Real incoming Regate MCP SDK -> curated tool -> outgoing
            // Regate MCP SDK -> actual third-party provider.
            gateway:=mcp.NewConn(proxy.URL+"/mcp/"+provider.name,"",35*time.Second)
            published,err:=gateway.ListTools(ctx)
            if err!=nil {t.Fatalf("Regate curated discovery: %v",err)}
            if len(published)!=1 || published[0]["name"]!=provider.published {
                t.Fatalf("Regate leaked or dropped curated tools: %v",published)
            }
            result,err:=gateway.CallTool(ctx,provider.published,provider.input)
            if err!=nil {t.Fatalf("Regate to public provider read: %v",err)}
            if result==nil || result["isError"]==true {
                t.Fatalf("read-only public tool did not succeed for %s",provider.name)
            }
            content,ok:=result["content"].([]any)
            structured,hasStructured:=result["structuredContent"]
            if (!ok || len(content)==0) && (!hasStructured || structured==nil) {
                t.Fatalf("empty read-only tool result from %s",provider.name)
            }
            // No provider payloads are printed or persisted to the test log.
            t.Logf("%s: anonymous HTTP MCP discovery + curated Regate tools/call succeeded",provider.name)
        })
    }
    t.Run("one-parallel-typed-tool-uses-both-providers",func(t *testing.T) {
        // In contrast with the separate curated proxies above, this makes a
        // SINGLE incoming MCP tools/call that traverses a two-provider DAG.
        // Never publish raw third-party content; only read-only block counts.
        readCtx,cancel:=context.WithTimeout(context.Background(),90*time.Second)
        defer cancel()
        gateway:=mcp.NewConn(proxy.URL+"/mcp/combined","",90*time.Second)
        published,err:=gateway.ListTools(readCtx)
        if err!=nil {t.Fatalf("combined catalog: %v",err)}
        if len(published)!=1 || published[0]["name"]!="public_docs_check" {
            t.Fatalf("combined surface contains unrelated tools: %v",published)
        }
        reply,err:=gateway.CallTool(readCtx,"public_docs_check",map[string]any{
            "query":"How do public Microsoft Learn MCP tools work?",
            "repoName":"modelcontextprotocol/servers",
        })
        if err!=nil || reply==nil || reply["isError"]==true {
            t.Fatalf("live two-provider DAG did not complete: %v (MCP application error=%t)",err,reply!=nil && reply["isError"]==true)
        }
        structured,ok:=reply["structuredContent"].(map[string]any)
        if !ok {t.Fatal("combined tool did not return structured output")}
        raw,err:=json.Marshal(structured)
        if err!=nil {t.Fatal(err)}
        var counts struct {
            Microsoft int `json:"microsoft_blocks"`
            DeepWiki int `json:"deepwiki_blocks"`
        }
        if err:=json.Unmarshal(raw,&counts);err!=nil {t.Fatalf("invalid composed output contract: %v",err)}
        if counts.Microsoft<=0 || counts.DeepWiki<=0 {
            t.Fatalf("one or both provider outputs missing: counts=%+v",counts)
        }
        seen:=map[string]int{}
        for _,tr:=range svc.Store.Traces() {
            if tr.Surface=="combined" && tr.Tool=="public_docs_check" &&
                tr.Status=="success" && (tr.Node=="docs" || tr.Node=="wiki") {
                seen[tr.Node]++
            }
        }
        if seen["docs"]!=1 || seen["wiki"]!=1 {
            t.Fatalf("composed DAG did not execute each independent read exactly once: %v",seen)
        }
        // Do not print any returned search/wiki documents.
        t.Log("one read-only Regate DAG completed two real independent provider calls with a typed all-join")
    })
    if effects:=svc.Store.Effects();len(effects)!=0 {
        t.Fatalf("read-only external acceptance unexpectedly journaled %d writes",len(effects))
    }
    if !strings.Contains(string(fixture),"read_only   = true") {
        t.Fatal("public provider fixture contains no explicit read-only tools")
    }
}
