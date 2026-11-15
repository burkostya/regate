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
    sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type sdkOwnerKey struct{}

type inboundEndpoint struct {
    server *sdk.Server
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
                s.mcp(w, r, surface, false)
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
    ep := &inboundEndpoint{server:srv}
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
                out, err := s.Engine.Run(ctx, *revision, cfg, surface, name, args, false)
                result := outcomeResult(out, err)
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
                    if rs.Source == "" {
                        return &sdk.ReadResourceResult{Contents:[]*sdk.ResourceContents{{URI:rs.URI, MIMEType:rs.MimeType, Text:rs.Text}}}, nil
                    }
                    upstream, _, ok := strings.Cut(rs.Source, ".")
                    if !ok { return nil, fmt.Errorf("invalid resource source") }
                    ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
                    defer cancel()
                    value, err := s.Engine.ReadResource(ctx, cfg.Upstreams[upstream], rs.URI)
                    if err != nil { return nil, err }
                    var out sdk.ReadResourceResult
                    if err := remarshalSDK(value, &out); err != nil { return nil, err }
                    return &out, nil
                }
            srv.AddResource(descriptor, handler)
            ep.notifyResources = func() { srv.AddResource(descriptor, handler) }
        }
        for _, prompt := range surf.Prompts {
            p := prompt
            descriptor := &sdk.Prompt{Name:p.Name, Description:p.Description}
            handler := func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
                    return &sdk.GetPromptResult{Messages:[]*sdk.PromptMessage{{Role:"user", Content:&sdk.TextContent{Text:p.Text}}}}, nil
                }
            srv.AddPrompt(descriptor, handler)
            ep.notifyPrompts = func() { srv.AddPrompt(descriptor, handler) }
        }
    }
    if err := registerTaskMethods(srv, s); err != nil { return nil, err }
    ep.handler = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv },
        &sdk.StreamableHTTPOptions{Stateless:true, JSONResponse:true, MaxRequestBodyBytes:4<<20, PropagateRequestCancellation:true})
    return ep, nil
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
}
type skillListResult struct {
    sdk.ResultBase
    Skills []map[string]any `json:"skills"`
    TTLMS int `json:"ttlMs"`
    CacheScope string `json:"cacheScope"`
}
type skillGetResult struct {
    sdk.ResultBase
    Skill map[string]any `json:"skill"`
    TTLMS int `json:"ttlMs"`
    CacheScope string `json:"cacheScope"`
}
type taskExtensionResult struct {
    sdk.ResultBase
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
func registerSkills(srv *sdk.Server, s *Service) error {
    if err := sdk.AddReceivingCustomMethod(srv,"skills/list",func(ctx context.Context, _ *sdk.ServerSession, _ *extensionParams) (*skillListResult,error) {
        return &skillListResult{Skills:s.Skills(), TTLMS:300000, CacheScope:"private"},nil
    }); err!=nil { return err }
    return sdk.AddReceivingCustomMethod(srv,"skills/get",func(ctx context.Context, _ *sdk.ServerSession,p *extensionParams) (*skillGetResult,error) {
        v,err:=s.Skill(p.URI)
        if err!=nil { return nil,err }
        var res skillGetResult
        if err:=remarshalSDK(v,&res); err!=nil {return nil,err}
        return &res,nil
    })
}
func registerTaskMethods(srv *sdk.Server, s *Service) error {
    methods:=[]struct{name string; fn func(string,string,map[string]any)(map[string]any,error)}{
        {"tasks/get",func(id,owner string,_ map[string]any)(map[string]any,error){return s.TaskGet(id,owner)}},
        {"tasks/update",func(id,owner string,r map[string]any)(map[string]any,error){return s.TaskUpdate(id,owner,r)}},
        {"tasks/cancel",func(id,owner string,_ map[string]any)(map[string]any,error){return s.TaskCancel(id,owner)}},
    }
    for _,method:=range methods {
        fn:=method.fn
        if err:=sdk.AddReceivingCustomMethod(srv,method.name,func(ctx context.Context, _ *sdk.ServerSession,p *extensionParams)(*taskExtensionResult,error){
            owner,_:=ctx.Value(sdkOwnerKey{}).(string)
            v,err:=fn(p.TaskID,owner,p.InputResponses)
            if err!=nil {return nil,err}
            var result taskExtensionResult
            if err:=remarshalSDK(v,&result); err!=nil {return nil,err}
            return &result,nil
        });err!=nil{return err}
    }
    return nil
}

