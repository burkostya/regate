package mcp

import (
    "context"
    "fmt"

    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxCatalogItems = 10000

// Catalog is an immutable discovery snapshot of one upstream MCP session.
// It is diagnostic, not proof that an untrusted upstream's schemas are safe.
type Catalog struct {
    Tools []map[string]any `json:"tools"`
    Resources []map[string]any `json:"resources"`
    ResourceTemplates []map[string]any `json:"resourceTemplates"`
    Prompts []map[string]any `json:"prompts"`
}

// collectCatalog follows all upstream pages with a bounded item count and
// refuses repeated cursors, including a first-page self-loop.
func collectCatalog[T any](method string, fetch func(string) ([]T, string, error)) ([]map[string]any, error) {
    results := []map[string]any{}
    seen := map[string]bool{"":true}
    cursor := ""
    for page:=0; page<maxToolPages; page++ {
        items, next, err := fetch(cursor)
        if err != nil { return nil,fmt.Errorf("%s page %d: %w",method,page+1,err) }
        if len(items) > maxCatalogItems-len(results) {
            return nil,fmt.Errorf("%s exceeds %d entries",method,maxCatalogItems)
        }
        for _, item := range items {
            var value map[string]any
            if err:=remarshal(item,&value); err!=nil { return nil,fmt.Errorf("%s schema: %w",method,err) }
            if value == nil { return nil,fmt.Errorf("%s returned null entry",method) }
            results=append(results,value)
        }
        if next=="" {return results,nil}
        if seen[next] {return nil,fmt.Errorf("%s repeated cursor %q",method,next)}
        seen[next]=true
        cursor=next
    }
    return nil,fmt.Errorf("%s exceeded %d pages",method,maxToolPages)
}

func allTools(ctx context.Context, session *sdk.ClientSession) ([]map[string]any,error) {
    return collectCatalog("tools/list",func(cursor string)([]*sdk.Tool,string,error) {
        r,err:=session.ListTools(ctx,&sdk.ListToolsParams{Cursor:cursor})
        if err!=nil {return nil,"",err}
        return r.Tools,r.NextCursor,nil
    })
}
func allResources(ctx context.Context, session *sdk.ClientSession) ([]map[string]any,error) {
    return collectCatalog("resources/list",func(cursor string)([]*sdk.Resource,string,error) {
        r,err:=session.ListResources(ctx,&sdk.ListResourcesParams{Cursor:cursor})
        if err!=nil {return nil,"",err}
        return r.Resources,r.NextCursor,nil
    })
}
func allResourceTemplates(ctx context.Context, session *sdk.ClientSession) ([]map[string]any,error) {
    return collectCatalog("resources/templates/list",func(cursor string)([]*sdk.ResourceTemplate,string,error) {
        r,err:=session.ListResourceTemplates(ctx,&sdk.ListResourceTemplatesParams{Cursor:cursor})
        if err!=nil {return nil,"",err}
        return r.ResourceTemplates,r.NextCursor,nil
    })
}
func allPrompts(ctx context.Context, session *sdk.ClientSession) ([]map[string]any,error) {
    return collectCatalog("prompts/list",func(cursor string)([]*sdk.Prompt,string,error) {
        r,err:=session.ListPrompts(ctx,&sdk.ListPromptsParams{Cursor:cursor})
        if err!=nil {return nil,"",err}
        return r.Prompts,r.NextCursor,nil
    })
}

// DiscoverCatalog uses a single SDK session for all collections so they are
// observed under one negotiated version and upstream principal. Capabilities
// guard optional families; tools/list stays callable for older deployments
// that implement it but omit the tools capability in server/discover.
func (c *Conn) DiscoverCatalog(ctx context.Context) (Catalog,error) {
    return withSession(c,ctx,func(session *sdk.ClientSession)(Catalog,error) {
        cat:=Catalog{
            Tools:[]map[string]any{},
            Resources:[]map[string]any{},
            ResourceTemplates:[]map[string]any{},
            Prompts:[]map[string]any{},
        }
        var err error
        if cat.Tools,err=allTools(ctx,session);err!=nil {return Catalog{},err}
        info:=session.InitializeResult()
        if info==nil || info.Capabilities==nil {return cat,nil}
        if info.Capabilities.Resources!=nil {
            if cat.Resources,err=allResources(ctx,session);err!=nil {return Catalog{},err}
            if cat.ResourceTemplates,err=allResourceTemplates(ctx,session);err!=nil {return Catalog{},err}
        }
        if info.Capabilities.Prompts!=nil {
            if cat.Prompts,err=allPrompts(ctx,session);err!=nil {return Catalog{},err}
        }
        return cat,nil
    })
}
