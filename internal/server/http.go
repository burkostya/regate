package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/mcp"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { reply(w, 200, map[string]any{"ok": true}) })
    mux.HandleFunc("/.well-known/oauth-protected-resource/",s.oauthResourceMetadata)
	mux.Handle("/api/", s.requireAdmin(http.HandlerFunc(s.api)))
	mux.Handle("/mcp/admin", s.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.inbound.handle(s, w, r, "", true) })))
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
        // Snapshot-scoped authentication is enforced by inbound.handle after
        // selecting the exact active SDK endpoint; an earlier global bearer
        // check could incorrectly reject valid OIDC credentials.
		c, _ := s.Current()
		surface := ""
		for name, s := range c.Surfaces {
			if r.URL.Path == s.Path {
				surface = name
				break
			}
		}
		if surface == "" {
			http.NotFound(w, r)
			return
		}
		s.inbound.handle(s, w, r, surface, false)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<!doctype html><html lang="en"><meta charset="utf-8"><title>Regate</title><style>body{font-family:system-ui;margin:6rem auto;max-width:48rem;padding:1rem;color:#263246}a{color:#365f91}code{background:#eee;padding:.2em .4em}</style><h1>Regate Gateway</h1><p>The MCP gateway is running. The SvelteKit/Svelte Flow console is a separate development service.</p><p>Start it with <code>make web-dev</code>, then open <a href="http://localhost:5173">localhost:5173</a>.</p><p>Health: <a href="/healthz">/healthz</a>.</p></html>`)
	})
	return mux
}
func constantTimeEq(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func bearer(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}
func (s *Service) authorize(r *http.Request, admin bool) bool {
	if admin {
		return constantTimeEq(bearer(r), AdminToken())
	}
	public := os.Getenv("REGATE_PUBLIC_TOKEN")
	if public != "" {
		return constantTimeEq(bearer(r), public)
	}
	// Anonymous access is permitted only for loopback clients.
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
func (s *Service) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorize(r, true) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="Regate admin"`)
			http.Error(w, "admin authentication required", 401)
			return
		}
		next.ServeHTTP(w, r)
	})
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errorJSON(w http.ResponseWriter, status int, e error) {
	reply(w, status, map[string]any{"error": e.Error()})
}
func parseJSON(r *http.Request) (map[string]any, error) {
    defer r.Body.Close()
    return strictJSONObject(r.Body)
}
func (s *Service) api(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" && r.Method != "PUT" {
		http.Error(w, "method not allowed", 405)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/api/")
	cfg, rev := s.Current()
	if r.Method == "GET" {
		switch p {
		case "status":
			reply(w, 200, s.Status())
			return
		case "config":
			reply(w, 200, map[string]any{"revision": rev.ID, "source": cfg.Source})
			return
		case "catalog":
			reply(w, 200, map[string]any{"upstreams": cfg.Upstreams, "surfaces": cfg.Surfaces, "pipelines": cfg.Pipelines, "hooks": cfg.Hooks, "policies": cfg.Policies, "interceptors": cfg.Interceptors})
			return
		case "revisions":
			reply(w, 200, s.Store.History())
			return
		case "traces":
            query,err:=parseTraceQuery(r.URL.Query())
            if err!=nil {
                errorJSON(w,400,err)
                return
            }
			reply(w, 200, s.Store.TracesFiltered(query))
			return
		case "effects":
			reply(w, 200, s.Store.Effects())
			return
        case "queue-key-usage":
            usage,e:=s.Store.QueueKeyUsage(r.Context())
            if e!=nil {errorJSON(w,400,e)} else {reply(w,200,usage)}
            return
        case "grant-key-usage":
            usage,e:=s.Store.GrantKeyUsage(r.Context())
            if e!=nil {errorJSON(w,400,e)} else {reply(w,200,usage)}
            return
		}
		if strings.HasPrefix(p, "drafts/") {
			d, e := s.Store.DraftGet(strings.TrimPrefix(p, "drafts/"))
			if e != nil {
				errorJSON(w, 404, e)
			} else {
				reply(w, 200, d)
			}
			return
		}
		if strings.HasPrefix(p, "graph/") {
			n := strings.TrimPrefix(p, "graph/")
			v, ok := cfg.Pipelines[n]
			if !ok {
				http.NotFound(w, r)
			} else {
				reply(w, 200, v)
			}
			return
		}
		if strings.HasPrefix(p, "layout/") {
			n := strings.TrimPrefix(p, "layout/")
			v, e := s.Store.Layout(n)
			if e != nil {
				errorJSON(w, 400, e)
			} else {
				reply(w, 200, v)
			}
			return
		}
	}
	if r.Method == "PUT" && strings.HasPrefix(p, "layout/") {
		v, e := parseJSON(r)
		if e != nil {
			errorJSON(w, 400, e)
			return
		}
		if e = s.Store.SaveLayout(strings.TrimPrefix(p, "layout/"), v); e != nil {
			errorJSON(w, 400, e)
			return
		}
		reply(w, 200, map[string]any{"saved": true})
		return
	}
	if r.Method == "POST" {
		v, e := parseJSON(r)
		if e != nil {
			errorJSON(w, 400, e)
			return
		}
		adminName := map[string]string{"drafts": "regate_draft_create", "draft-update": "regate_draft_patch", "validate": "regate_validate", "diff": "regate_diff", "impact": "regate_impact_analyze", "activate": "regate_activate", "rollback": "regate_rollback", "test": "regate_test", "simulate": "regate_simulate", "run": "regate_run", "discover": "regate_upstream_refresh", "effect-decide": "regate_effect_decide", "effect-readback": "regate_effect_readback", "effect-compensate": "regate_effect_compensate", "task-resume-grant": "regate_task_resume_grant", "queue-rewrap": "regate_queue_rewrap", "grant-rewrap": "regate_grant_rewrap", "resume": "regate_resume", "graph-patch": "regate_graph_patch", "attribute-patch": "regate_attribute_patch", "attribute-add": "regate_attribute_add"}[p]
		if adminName != "" {
			result, e := s.Admin(r.Context(), adminName, v)
			if e != nil {
				errorJSON(w, 400, e)
			} else {
				reply(w, 200, result)
			}
			return
		}
	}
	http.NotFound(w, r)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any, latest bool) {
	if latest {
		if m, ok := result.(map[string]any); ok {
			if _, has := m["resultType"]; !has {
				m["resultType"] = "complete"
			}
		}
	}
	reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}
func rpcError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "error": map[string]any{"code": code, "message": msg}})
}
func (s *Service) mcp(w http.ResponseWriter, r *http.Request, surface string, admin bool) {
	if r.Method != "POST" {
		http.Error(w, "MCP requires HTTP POST", 405)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "content-type must be application/json", 415)
		return
	}
	defer r.Body.Close()
	var q request
	if e := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&q); e != nil {
		rpcError(w, json.RawMessage("null"), -32700, "invalid JSON")
		return
	}
	if q.JSONRPC != "2.0" || q.Method == "" {
		rpcError(w, q.ID, -32600, "invalid JSON-RPC request")
		return
	}
	if len(q.ID) == 0 {
		w.WriteHeader(202)
		return
	}
	latest := r.Header.Get("Mcp-Protocol-Version") == mcp.Protocol
	if latest && q.Method != "server/discover" {
		if q.Params == nil {
			rpcError(w, q.ID, -32602, "missing _meta")
			return
		}
		meta, _ := q.Params["_meta"].(map[string]any)
		if meta["io.modelcontextprotocol/protocolVersion"] != mcp.Protocol {
			rpcError(w, q.ID, -32602, "protocol metadata mismatch")
			return
		}
	}
	if latest {
		if expected := r.Header.Get("Mcp-Method"); expected != "" && expected != q.Method {
			rpcError(w, q.ID, -32020, "Mcp-Method does not match JSON-RPC method")
			return
		}
		if expected := r.Header.Get("Mcp-Name"); expected != "" {
			name := str(q.Params, "name")
			if strings.HasPrefix(q.Method, "tasks/") {
				name = str(q.Params, "taskId")
			}
			if name != "" && name != expected {
				rpcError(w, q.ID, -32020, "Mcp-Name does not match params")
				return
			}
		}
	}
	if latest && q.Method == "subscriptions/listen" {
		s.listen(w, r, q.ID, q.Params)
		return
	}
	if latest && strings.HasPrefix(q.Method, "tasks/") {
		var result map[string]any
		var e error
		switch q.Method {
		case "tasks/get":
			result, e = s.TaskGet(str(q.Params, "taskId"), bearer(r))
		case "tasks/update":
			result, e = s.TaskUpdate(str(q.Params, "taskId"), bearer(r), obj(q.Params, "inputResponses"))
		case "tasks/cancel":
			result, e = s.TaskCancel(str(q.Params, "taskId"), bearer(r))
		default:
			rpcError(w, q.ID, -32601, "unsupported task operation")
			return
		}
		if e != nil {
			rpcError(w, q.ID, -32602, e.Error())
			return
		}
		rpcResult(w, q.ID, result, true)
		return
	}
	if latest && !admin && q.Method == "tools/call" && supportsTasks(q.Params) {
		result, e := s.CreateTask(r.Context(),surface, str(q.Params, "name"), bearer(r), obj(q.Params, "arguments"))
		if e != nil {
			rpcError(w, q.ID, -32000, e.Error())
			return
		}
		rpcResult(w, q.ID, result, true)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	result, e := s.dispatchMCP(ctx, surface, admin, q.Method, q.Params, latest)
	if e != nil {
		var rpcErr *mcp.RPCError
		if errors.As(e, &rpcErr) {
			rpcError(w, q.ID, rpcErr.Code, rpcErr.Message)
		} else {
			rpcError(w, q.ID, -32000, e.Error())
		}
		return
	}
	rpcResult(w, q.ID, result, latest)
}
func (s *Service) dispatchMCP(ctx context.Context, surface string, admin bool, method string, params map[string]any, latest bool) (map[string]any, error) {
	cfg, rev := s.Current()
	switch method {
	case "server/discover":
		return map[string]any{"supportedVersions": []string{mcp.Protocol, mcp.LegacyProtocol}, "serverInfo": map[string]any{"name": "regate", "version": "0.1.0"}, "capabilities": discoveryCaps(admin), "instructions": discoveryInstructions(admin)}, nil
	case "initialize":
		return map[string]any{"protocolVersion": mcp.LegacyProtocol, "serverInfo": map[string]any{"name": "regate", "version": "0.1.0"}, "capabilities": map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{}, "prompts": map[string]any{}}, "instructions": "Regate HTTP MCP gateway"}, nil
	case "tools/list":
		if admin {
			return map[string]any{"tools": AdminDefs()}, nil
		}
		tools := []any{}
		s := cfg.Surfaces[surface]
		for _, t := range s.Tools {
			input := t.InputSchema
			if input == nil {
				input = map[string]any{"type": "object", "additionalProperties": true}
			}
			obj := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": input}
			if t.OutputSchema != nil {
				obj["outputSchema"] = t.OutputSchema
			}
			tools = append(tools, obj)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		name := str(params, "name")
		args := obj(params, "arguments")
		var out any
		var err error
		if admin {
			if _, ok := AdminTools[name]; !ok {
				return nil, &mcp.RPCError{Code: -32602, Message: "unknown admin tool"}
			}
			out, err = s.Admin(ctx, name, args)
		} else {
			out, err = s.Engine.Run(ctx, rev.ID, cfg, surface, name, args, false)
		}
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		if v, ok := out.(engine.Outcome); ok {
			return outcomeResult(v, nil), nil
		}
		b, _ := json.Marshal(out)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}, "structuredContent": out, "isError": false}, nil
	case "resources/list":
		results := []any{}
		if admin {
			for _, skill := range s.Skills() {
				front, _ := skill["frontmatter"].(map[string]any)
				results = append(results, map[string]any{"uri": skill["uri"], "name": front["name"], "mimeType": "text/markdown"})
			}
		} else {
			for _, res := range cfg.Surfaces[surface].Resources {
				results = append(results, map[string]any{"uri": res.URI, "name": res.Name, "mimeType": res.MimeType})
			}
		}
		return map[string]any{"resources": results}, nil
	case "resources/read":
		uri := str(params, "uri")
		if admin && strings.HasPrefix(uri, "skill://") {
			content, e := s.SkillContent(uri)
			if e != nil {
				return nil, &mcp.RPCError{Code: -32602, Message: e.Error()}
			}
			return map[string]any{"contents": []any{map[string]any{"uri": uri, "mimeType": "text/markdown", "text": content}}, "ttlMs": 300000, "cacheScope": "private"}, nil
		}
		for _, rs := range cfg.Surfaces[surface].Resources {
			if rs.URI != uri {
				continue
			}
			if rs.Source != "" {
				upstream, _, ok := strings.Cut(rs.Source, ".")
				if !ok {
					return nil, fmt.Errorf("invalid resource source")
				}
				u := cfg.Upstreams[upstream]
				return s.Engine.ReadResource(ctx, cfg, u, uri)
			}
			return map[string]any{"contents": []any{map[string]any{"uri": uri, "mimeType": rs.MimeType, "text": rs.Text}}}, nil
		}
		return nil, &mcp.RPCError{Code: -32602, Message: "unknown resource"}
	case "prompts/list":
		prompts := []any{}
		if !admin {
			for _, p := range cfg.Surfaces[surface].Prompts {
				prompts = append(prompts, map[string]any{"name": p.Name, "description": p.Description})
			}
		}
		return map[string]any{"prompts": prompts}, nil
	case "prompts/get":
		for _, p := range cfg.Surfaces[surface].Prompts {
			if p.Name == str(params, "name") {
				return map[string]any{"messages": []any{map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": p.Text}}}}, nil
			}
		}
		return nil, &mcp.RPCError{Code: -32602, Message: "unknown prompt"}
	case "skills/list":
		if admin {
			return map[string]any{"skills": s.Skills(), "ttlMs": 300000, "cacheScope": "private"}, nil
		}
		return nil, &mcp.RPCError{Code: -32601, Message: "skills unavailable on public surface"}
	case "skills/get":
		if !admin {
			return nil, &mcp.RPCError{Code: -32601, Message: "skills unavailable on public surface"}
		}
		entry, e := s.Skill(str(params, "uri"))
		if e != nil {
			return nil, &mcp.RPCError{Code: -32602, Message: e.Error()}
		}
		return entry, nil
	}
	return nil, &mcp.RPCError{Code: -32601, Message: "method not implemented: " + method}
}
func (s *Service) Serve(ctx context.Context, addr string) error {
	httpSrv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 8 * time.Second, IdleTimeout: 75 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	log.Printf("regate listening on %s", addr)
	e := httpSrv.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func (s *Service) DebugConfig() string { c, _ := s.Current(); return configSummary(c) }
func configSummary(c *config.Config) string {
	return fmt.Sprintf("%d upstreams, %d pipelines, %d surfaces", len(c.Upstreams), len(c.Pipelines), len(c.Surfaces))
}

func supportsTasks(params map[string]any) bool {
	meta, _ := params["_meta"].(map[string]any)
	caps, _ := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any)
	ext, _ := caps["extensions"].(map[string]any)
	_, ok := ext["io.modelcontextprotocol/tasks"]
	return ok
}
func discoveryCaps(admin bool) map[string]any {
	caps := map[string]any{"tools": map[string]any{"listChanged": true}, "resources": map[string]any{"listChanged": true}, "prompts": map[string]any{"listChanged": true}, "extensions": map[string]any{"io.modelcontextprotocol/tasks": map[string]any{}}}
	if admin {
		caps["extensions"].(map[string]any)["io.modelcontextprotocol/skills"] = map[string]any{}
	}
	return caps
}
func discoveryInstructions(admin bool) string {
	if admin {
		return "Regate management MCP. Discover instructions via skill://regate-config/SKILL.md and skill://regate-authoring/SKILL.md. Follow draft -> validate -> diff -> test -> activate."
	}
	return "Regate virtual MCP surface. Tool calls may use tasks when the client supports the tasks extension."
}
