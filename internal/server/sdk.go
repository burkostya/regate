package server

// Incoming MCP protocol handling is delegated to the official Go SDK. Regate's
// revision snapshots are separate servers: in-flight calls retain their config,
// while new requests see the activated revision. The narrow task-start fallback
// below exists because the SDK's CallToolResult does not represent the tasks
// extension's resultType="task" response.
import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "strings"
    "sync"
    "time"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/identity"
    "github.com/burkostya/regate/internal/expr"
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
    "github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

type sdkOwnerKey struct{}

type inboundEndpoint struct {
    server *sdk.Server
    verifier *identity.Verifier
    requiredScopes []string
    toolScopes map[string][]string
    config *config.Config
    revision *string
    resourceMetadata string
    handler http.Handler
    // Re-registering an unchanged descriptor triggers SDK list_changed without
    // deleting the old immutable catalog, including while requests are in flight.
    notifyTools func()
    notifyResources func()
    notifyPrompts func()
}

type inboundMCP struct {
    mu sync.RWMutex
    admin *inboundEndpoint
    public map[string]*inboundEndpoint
}

func newInboundMCP(s *Service, cfg *config.Config, revision string) (*inboundMCP, error) {
    m := &inboundMCP{public: make(map[string]*inboundEndpoint)}
    admin, err := newInboundEndpoint(s, cfg, &revision, "", true)
    if err != nil { return nil, err }
    m.admin = admin
    if err := m.replace(s, cfg, revision); err != nil { return nil, err }
    return m, nil
}

// prepare builds the next immutable catalogs before the store transaction.
// No route can observe them until publish has installed the real revision ID.
func (m *inboundMCP) prepare(s *Service, cfg *config.Config) (map[string]*inboundEndpoint, *string, error) {
    next := make(map[string]*inboundEndpoint, len(cfg.Surfaces))
    revision := new(string)
    for name := range cfg.Surfaces {
        ep, err := newInboundEndpoint(s, cfg, revision, name, false)
        if err != nil { return nil, nil, fmt.Errorf("MCP surface %q: %w", name, err) }
        next[name] = ep
    }
    return next, revision, nil
}

func (m *inboundMCP) replace(s *Service, cfg *config.Config, revision string) error {
    next, rev, err := m.prepare(s, cfg)
    if err != nil { return err }
    *rev = revision
    m.publish(next)
    return nil
}
func (m *inboundMCP) publish(next map[string]*inboundEndpoint) {
    m.mu.Lock()
    previous := m.public
    m.public = next
    m.mu.Unlock()

    // SDK-native subscriptions continue on their original sessions. Trigger a
    // catalog refresh without deleting descriptors from in-flight revisions.
    for _, old := range previous {
        if old.notifyTools != nil { old.notifyTools() }
        if old.notifyResources != nil { old.notifyResources() }
        if old.notifyPrompts != nil { old.notifyPrompts() }
    }
}

// handle preserves the old, unversioned HTTP JSON-RPC client's missing Accept
// header, while the SDK enforces all other protocol framing and validation.
func (m *inboundMCP) handle(s *Service, w http.ResponseWriter, r *http.Request, surface string, admin bool) {
    m.mu.RLock()
    ep := m.admin
    if !admin { ep = m.public[surface] }
    m.mu.RUnlock()
    if ep == nil {
        http.NotFound(w, r)
        return
    }
    if !admin {
        if ep.verifier!=nil {
            p,err:=ep.verifier.Verify(r.Context(),bearer(r))
            if err!=nil {
                w.Header().Set("WWW-Authenticate",wwwAuthenticate(ep.resourceMetadata,"invalid_token"))
                http.Error(w,"invalid access token",http.StatusUnauthorized)
                return
            }
            if !p.HasAll(ep.requiredScopes) {
                w.Header().Set("WWW-Authenticate",wwwAuthenticate(ep.resourceMetadata,"insufficient_scope"))
                http.Error(w,"insufficient scope",http.StatusForbidden)
                return
            }
            verified:=identity.WithPrincipal(r.Context(),p)
            r=r.WithContext(identity.WithSubjectToken(verified,bearer(r)))
        } else if !s.authorize(r,false) {
            w.Header().Set("WWW-Authenticate", "Bearer")
            http.Error(w,"unauthorized",http.StatusUnauthorized)
            return
        }
    }

    // The SDK doesn't expose a task-start result type. Keep precisely this
    // extension's existing wire contract until SDK support is available.
    // Regate's pre-SDK HTTP clients used server/discover without advertising
    // the new protocol. Keep only this legacy handshake on the old dispatcher.
    // Versioned discovery, standard methods and subscriptions are SDK-owned.
    version := r.Header.Get("Mcp-Protocol-Version")
    if r.Method == http.MethodPost && (version == "" || version == "2026-07-28") {
        body, err := io.ReadAll(io.LimitReader(r.Body, (4<<20)+1))
        if err != nil { http.Error(w, "invalid MCP request body", http.StatusBadRequest); return }
        _ = r.Body.Close()
        if len(body) > 4<<20 { http.Error(w, "MCP request exceeds 4 MiB", http.StatusRequestEntityTooLarge); return }
        r.Body = io.NopCloser(bytes.NewReader(body))
        var q request
        if json.Unmarshal(body, &q) == nil {
            if version == "" && q.Method == "server/discover" {
                s.mcp(w, r, surface, admin)
                return
            }
            if version == "2026-07-28" && !admin && q.Method == "tools/call" && supportsTasks(q.Params) {
                // The compatibility branch must not bypass the mandatory
                // 2026-07-28 standard header checks enforced by the SDK.
                name := str(q.Params, "name")
                accept := r.Header.Get("Accept")
                if name == "" || r.Header.Get("Mcp-Method") != "tools/call" ||
                    r.Header.Get("Mcp-Name") != name ||
                    !strings.Contains(accept, "application/json") ||
                    !strings.Contains(accept, "text/event-stream") {
                    http.Error(w, "invalid MCP task-start headers", http.StatusBadRequest)
                    return
                }
                if err:=identity.RequireScopes(r.Context(),ep.toolScopes[name]);err!=nil {
                    rpcError(w,q.ID,-32001,"insufficient tool scope")
                    return
                }
                if q.JSONRPC!="2.0" || len(q.ID)==0 || string(q.ID)=="null" {
                    rpcError(w,q.ID,-32600,"invalid task-start request")
                    return
                }
                meta,_:=q.Params["_meta"].(map[string]any)
                if meta["io.modelcontextprotocol/protocolVersion"]!="2026-07-28" {
                    rpcError(w,q.ID,-32602,"invalid protocol metadata")
                    return
                }
                if _,ok:=meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any);!ok {
                    rpcError(w,q.ID,-32602,"invalid client capabilities")
                    return
                }
                result,err:=s.createTaskFor(r.Context(),ep.config,*ep.revision,surface,name,bearer(r),obj(q.Params,"arguments"))
                if err!=nil {
                    rpcError(w,q.ID,-32000,"task rejected")
                    return
                }
                rpcResult(w,q.ID,result,true)
                return
            }
        }
    }

    // Older Regate clients omitted Accept (equivalent to */* in net/http).
    // Do not alter a supplied restrictive Accept header or protocol metadata.
    if r.Header.Get("Accept") == "" || r.Header.Get("Accept") == "*/*" {
        r = r.Clone(r.Context())
        r.Header = r.Header.Clone()
        r.Header.Set("Accept", "application/json, text/event-stream")
    }
    ep.handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sdkOwnerKey{}, bearer(r))))
}

func newInboundEndpoint(s *Service, cfg *config.Config, revision *string, surface string, admin bool) (*inboundEndpoint, error) {
    caps := &sdk.ServerCapabilities{
        Tools: &sdk.ToolCapabilities{ListChanged: true},
        Resources: &sdk.ResourceCapabilities{ListChanged: true},
        Prompts: &sdk.PromptCapabilities{ListChanged: true},
    }
    caps.AddExtension("io.modelcontextprotocol/tasks", nil)
    if admin { caps.AddExtension("io.modelcontextprotocol/skills", nil) }
    srv := sdk.NewServer(&sdk.Implementation{Name:"regate", Version:"0.1.0"},
        &sdk.ServerOptions{Instructions:discoveryInstructions(admin), Capabilities:caps, PageSize:128})
    ep := &inboundEndpoint{server:srv,config:cfg,revision:revision}
    if !admin {
        surf:=cfg.Surfaces[surface]
        ep.requiredScopes=surf.RequiredScopes
        ep.toolScopes=make(map[string][]string,len(surf.Tools))
        for _,tool:=range surf.Tools {ep.toolScopes[tool.Name]=tool.RequiredScopes}
        if surf.OAuth!="" {
            provider:=cfg.OAuth[surf.OAuth]
            ep.verifier=identity.NewVerifier(provider)
            ep.resourceMetadata=resourceMetadataURL(provider,surf)
        }
        registerVisibilityMiddleware(srv,surf)
    }
    if admin {
        for _, def := range AdminDefs() {
            var tool sdk.Tool
            if err := remarshalSDK(def, &tool); err != nil { return nil, err }
            name := tool.Name
            handler := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
                args, err := sdkArgs(req.Params.Arguments)
                if err != nil { return nil, err }
                ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
                defer cancel()
                v, err := s.Admin(ctx, name, args)
                if err != nil { return sdkToolError(err), nil }
                return sdkToolValue(v)
            }
            srv.AddTool(&tool, handler)
            ep.notifyTools = func() { srv.AddTool(&tool, handler) }
        }
        for _, skill := range s.Skills() {
            uri, _ := skill["uri"].(string)
            front, _ := skill["frontmatter"].(map[string]any)
            name, _ := front["name"].(string)
            if uri == "" { continue }
            descriptor := &sdk.Resource{URI:uri, Name:name, MIMEType:"text/markdown"}
            handler := func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
                    content, err := s.SkillContent(req.Params.URI)
                    if err != nil { return nil, err }
                    return &sdk.ReadResourceResult{Contents:[]*sdk.ResourceContents{{URI:req.Params.URI, MIMEType:"text/markdown", Text:content}}}, nil
                }
            srv.AddResource(descriptor, handler)
            ep.notifyResources = func() { srv.AddResource(descriptor, handler) }
        }
        if err := registerSkills(srv, s); err != nil { return nil, err }
    } else {
        surf := cfg.Surfaces[surface]
        for _, tool := range surf.Tools {
            schema := tool.InputSchema
            if schema == nil { schema = map[string]any{"type":"object", "additionalProperties":true} }
            desc := &sdk.Tool{Name:tool.Name, Description:tool.Description, InputSchema:schema, OutputSchema:tool.OutputSchema}
            name := tool.Name
            handler := func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
                args, err := sdkArgs(req.Params.Arguments)
                if err != nil { return nil, err }
                ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
                defer cancel()
                if err:=identity.RequireScopes(ctx,tool.RequiredScopes);err!=nil {
                    return sdkToolError(err),nil
                }
                out, err := s.Engine.Run(ctx, *revision, cfg, surface, name, args, false)
                result := outcomeResult(out, err)
                if ep.verifier!=nil && err!=nil {
                    result=map[string]any{"content":[]any{map[string]any{"type":"text","text":"operation failed"}},"isError":true}
                }
                if ep.verifier!=nil && !tool.AllowUnmasked {
                    if scrubbed,ok:=expr.MaskPII(result).(map[string]any);ok {result=scrubbed}
                }
                var wire sdk.CallToolResult
                if err := remarshalSDK(result, &wire); err != nil { return nil, err }
                return &wire, nil
            }
            srv.AddTool(desc, handler)
            ep.notifyTools = func() { srv.AddTool(desc, handler) }
        }
        for _, resource := range surf.Resources {
            parsed, err := url.Parse(resource.URI)
            if err != nil || parsed.Scheme == "" { return nil, fmt.Errorf("resource URI %q must be absolute", resource.URI) }
            rs := resource
            descriptor := &sdk.Resource{URI:rs.URI, Name:rs.Name, MIMEType:rs.MimeType}
            handler := func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
                    if err:=identity.RequireScopes(ctx,rs.RequiredScopes);err!=nil {return nil,err}
                    if rs.Source == "" {
                        text:=rs.Text
                        if ep.verifier!=nil {text=expr.MaskPII(text).(string)}
                        return &sdk.ReadResourceResult{Contents:[]*sdk.ResourceContents{{URI:rs.URI, MIMEType:rs.MimeType, Text:text}}}, nil
                    }
                    upstream, _, ok := strings.Cut(rs.Source, ".")
                    if !ok { return nil, fmt.Errorf("invalid resource source") }
                    ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
                    defer cancel()
                    if ep.verifier!=nil && cfg.Upstreams[upstream].TrustLevel!="restricted" {
                        return nil,fmt.Errorf("untrusted upstream resource boundary")
                    }
                    value, err := s.Engine.ReadResource(ctx,cfg,cfg.Upstreams[upstream], rs.URI)
                    if err != nil { return nil, err }
                    var out sdk.ReadResourceResult
                    if err := remarshalSDK(value, &out); err != nil { return nil, err }
                    if ep.verifier!=nil {
                        for _,item:=range out.Contents {
                            if len(item.Blob)>0 {return nil,fmt.Errorf("binary resources require explicit declassification")}
                            item.Text=expr.MaskPII(item.Text).(string)
                        }
                    }
                    return &out, nil
                }
            srv.AddResource(descriptor, handler)
            ep.notifyResources = func() { srv.AddResource(descriptor, handler) }
        }
        for _, prompt := range surf.Prompts {
            p := prompt
            descriptor := &sdk.Prompt{Name:p.Name, Description:p.Description}
            handler := func(ctx context.Context, _ *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
                    if err:=identity.RequireScopes(ctx,p.RequiredScopes);err!=nil {return nil,err}
                    value:=p.Text
                    if ep.verifier!=nil {value=expr.MaskPII(value).(string)}
                    return &sdk.GetPromptResult{Messages:[]*sdk.PromptMessage{{Role:"user", Content:&sdk.TextContent{Text:value}}}}, nil
                }
            srv.AddPrompt(descriptor, handler)
            ep.notifyPrompts = func() { srv.AddPrompt(descriptor, handler) }
        }
    }
    if err := registerTaskMethods(srv, s, surface, admin); err != nil { return nil, err }
    ep.handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
        &sdk.StreamableHTTPOptions{Stateless:true, JSONResponse:true, MaxRequestBodyBytes:4<<20, PropagateRequestCancellation:true})
    return ep, nil
}

// Visibility is scoped by verified principal. This filters discovery catalogs
// as well as protecting individual calls; unauthorized resources cannot be
// enumerated by their URIs or tool names.
func registerVisibilityMiddleware(srv *sdk.Server,surface config.Surface) {
    tools:=map[string][]string{}
    resources:=map[string][]string{}
    prompts:=map[string][]string{}
    for _,t:=range surface.Tools {tools[t.Name]=t.RequiredScopes}
    for _,res:=range surface.Resources {resources[res.URI]=res.RequiredScopes}
    for _,p:=range surface.Prompts {prompts[p.Name]=p.RequiredScopes}
    srv.AddReceivingMiddleware(func(next sdk.MethodHandler) sdk.MethodHandler {
        return func(ctx context.Context,method string,req sdk.Request)(sdk.Result,error){
            result,err:=next(ctx,method,req)
            if err!=nil || result==nil {return result,err}
            principal,_:=identity.FromContext(ctx)
            switch typed:=result.(type) {
            case *sdk.ListToolsResult:
                kept:=make([]*sdk.Tool,0,len(typed.Tools))
                for _,tool:=range typed.Tools {if principal.HasAll(tools[tool.Name]) {kept=append(kept,tool)}}
                typed.Tools=kept
            case *sdk.ListResourcesResult:
                kept:=make([]*sdk.Resource,0,len(typed.Resources))
                for _,res:=range typed.Resources {if principal.HasAll(resources[res.URI]) {kept=append(kept,res)}}
                typed.Resources=kept
            case *sdk.ListPromptsResult:
                kept:=make([]*sdk.Prompt,0,len(typed.Prompts))
                for _,p:=range typed.Prompts {if principal.HasAll(prompts[p.Name]) {kept=append(kept,p)}}
                typed.Prompts=kept
            }
            return result,nil
        }
    })
}

func remarshalSDK(value any, out any) error {
    data, err := json.Marshal(value)
    if err != nil { return err }
    return json.Unmarshal(data, out)
}
func sdkArgs(raw json.RawMessage) (map[string]any, error) {
    if len(raw)==0 || string(raw)=="null" { return map[string]any{}, nil }
    var args map[string]any
    if err := json.Unmarshal(raw,&args); err!=nil { return nil, err }
    if args==nil { return nil, fmt.Errorf("tool arguments must be an object") }
    return args,nil
}
func sdkToolError(err error) *sdk.CallToolResult {
    return &sdk.CallToolResult{Content:[]sdk.Content{&sdk.TextContent{Text:err.Error()}}, IsError:true}
}
func sdkToolValue(v any) (*sdk.CallToolResult,error) {
    b, err := json.Marshal(v)
    if err!=nil { return nil,err }
    return &sdk.CallToolResult{Content:[]sdk.Content{&sdk.TextContent{Text:string(b)}}, StructuredContent:v}, nil
}

type extensionParams struct {
    sdk.ParamsBase
    URI string `json:"uri,omitempty"`
    TaskID string `json:"taskId,omitempty"`
    InputResponses map[string]any `json:"inputResponses,omitempty"`
    RefreshToken string `json:"refreshToken,omitempty"`
}
type skillListResult struct {
    sdk.ResultBase
    ResultType string `json:"resultType,omitempty"`
    Skills []map[string]any `json:"skills"`
    TTLMS int `json:"ttlMs"`
    CacheScope string `json:"cacheScope"`
}
type skillGetResult struct {
    sdk.ResultBase
    ResultType string `json:"resultType,omitempty"`
    Skill map[string]any `json:"skill"`
    TTLMS int `json:"ttlMs"`
    CacheScope string `json:"cacheScope"`
}
type taskExtensionResult struct {
    sdk.ResultBase
    ResultType string `json:"resultType,omitempty"`
    TaskID string `json:"taskId,omitempty"`
    Status string `json:"status,omitempty"`
    StatusMessage string `json:"statusMessage,omitempty"`
    CreatedAt string `json:"createdAt,omitempty"`
    LastUpdatedAt string `json:"lastUpdatedAt,omitempty"`
    TTLMS int64 `json:"ttlMs,omitempty"`
    PollIntervalMS int64 `json:"pollIntervalMs,omitempty"`
    Result map[string]any `json:"result,omitempty"`
    Error map[string]any `json:"error,omitempty"`
    InputRequests map[string]any `json:"inputRequests,omitempty"`
}
// Only an OAuth-authenticated task owner can provision, inspect or revoke
// a sealed refresh grant. This method family is not registered on admin MCP.
type grantExtensionResult struct {
    sdk.ResultBase
    ResultType string `json:"resultType,omitempty"`
    TaskID string `json:"taskId"`
    State string `json:"state"`
    ExpiresAt string `json:"expiresAt,omitempty"`
    Generation int64 `json:"generation,omitempty"`
}
func registerGrantMethods(srv *sdk.Server,s *Service,surface string)error {
    for _,name:=range []string{"tasks/grants/provision","tasks/grants/status","tasks/grants/revoke"} {
        operation:=name
        if err:=sdk.AddReceivingCustomMethod(srv,operation,
            func(ctx context.Context,_ *sdk.ServerSession,p *extensionParams)(*grantExtensionResult,error) {
                if p==nil || p.TaskID=="" {
                    return nil,&jsonrpc.Error{Code:-32602,Message:"invalid grant request"}
                }
                owner,_:=ctx.Value(sdkOwnerKey{}).(string)
                owner=ownerIdentity(ctx,owner)
                out:=&grantExtensionResult{ResultType:"complete",TaskID:p.TaskID}
                switch operation {
                case "tasks/grants/provision":
                    if len(p.RefreshToken)==0 || len(p.RefreshToken)>8192 {
                        return nil,&jsonrpc.Error{Code:-32602,Message:"invalid grant credential"}
                    }
                    meta,err:=s.ProvisionTaskGrant(ctx,surface,p.TaskID,owner,p.RefreshToken)
                    if err!=nil {
                        return nil,&jsonrpc.Error{Code:-32602,Message:"grant provisioning unavailable"}
                    }
                    out.State=meta.State
                    out.Generation=meta.Generation
                    out.ExpiresAt=meta.ExpiresAt.Format(time.RFC3339Nano)
                case "tasks/grants/status":
                    meta,err:=s.TaskGrantState(ctx,surface,p.TaskID,owner)
                    if err!=nil {
                        return nil,&jsonrpc.Error{Code:-32602,Message:"grant not found"}
                    }
                    out.State=meta.State
                    out.Generation=meta.Generation
                    out.ExpiresAt=meta.ExpiresAt.Format(time.RFC3339Nano)
                case "tasks/grants/revoke":
                    if err:=s.RevokeTaskGrant(ctx,surface,p.TaskID,owner);err!=nil {
                        return nil,&jsonrpc.Error{Code:-32602,Message:"grant not found"}
                    }
                    out.State="revoked"
                }
                return out,nil
            });err!=nil{return err}
    }
    return nil
}

func registerSkills(srv *sdk.Server, s *Service) error {
    if err := sdk.AddReceivingCustomMethod(srv,"skills/list",func(ctx context.Context, _ *sdk.ServerSession, _ *extensionParams) (*skillListResult,error) {
        return &skillListResult{ResultType:"complete", Skills:s.Skills(), TTLMS:300000, CacheScope:"private"},nil
    }); err!=nil { return err }
    return sdk.AddReceivingCustomMethod(srv,"skills/get",func(ctx context.Context, _ *sdk.ServerSession,p *extensionParams) (*skillGetResult,error) {
        v,err:=s.Skill(p.URI)
        if err!=nil { return nil,err }
        var res skillGetResult
        if err:=remarshalSDK(v,&res); err!=nil {return nil,err}
        res.ResultType = "complete"
        return &res,nil
    })
}
func registerTaskMethods(srv *sdk.Server, s *Service, surface string, admin bool) error {
    methods:=[]struct{name string; fn func(context.Context,string,string,map[string]any)(map[string]any,error)}{
        {"tasks/get",func(_ context.Context,id,owner string,_ map[string]any)(map[string]any,error){return s.TaskGet(id,owner)}},
        {"tasks/update",func(ctx context.Context,id,owner string,r map[string]any)(map[string]any,error){return s.TaskUpdateWithContext(ctx,id,owner,r)}},
        {"tasks/cancel",func(_ context.Context,id,owner string,_ map[string]any)(map[string]any,error){return s.TaskCancel(id,owner)}},
    }
    for _,method:=range methods {
        fn:=method.fn
        if err:=sdk.AddReceivingCustomMethod(srv,method.name,func(ctx context.Context, _ *sdk.ServerSession,p *extensionParams)(*taskExtensionResult,error){
            owner,_:=ctx.Value(sdkOwnerKey{}).(string)
            owner=ownerIdentity(ctx,owner)
            // Every task lookup is bound to both the original public surface
            // and its immutable creation-time scopes, not merely the bearer
            // string. A later scope revocation must fail closed.
            if !admin {
                record,err:=s.Store.TaskGet(p.TaskID)
                if err!=nil || record.Surface!=surface {return nil,&jsonrpc.Error{Code:-32602,Message:"task not found"}}
                if err:=identity.RequireScopes(ctx,record.RequiredScopes);err!=nil {return nil,&jsonrpc.Error{Code:-32001,Message:"insufficient task scope"}}
            }
            v,err:=fn(ctx,p.TaskID,owner,p.InputResponses)
            if err!=nil {return nil,&jsonrpc.Error{Code:-32602,Message:"task not found"}}
            var result taskExtensionResult
            if err:=remarshalSDK(v,&result); err!=nil {return nil,err}
            result.ResultType = "complete"
            return &result,nil
        });err!=nil{return err}
    }
    if !admin {return registerGrantMethods(srv,s,surface)}
    return nil
}

