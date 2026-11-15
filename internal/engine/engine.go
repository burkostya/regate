package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/expr"
	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/netguard"
	"github.com/burkostya/regate/internal/storage"
)

type Runtime struct {
	Store       *storage.Store
	mu          sync.Mutex
	connections map[string]*mcp.Conn
}

func New(store *storage.Store) *Runtime {
	return &Runtime{Store: store, connections: map[string]*mcp.Conn{}}
}
// connFor obtains a target-audience-specific delegated credential when the
// upstream requires RFC8693 exchange. It must not cache delegated bearers.
func (r *Runtime) connFor(ctx context.Context,cfg *config.Config,u config.Upstream) (*mcp.Conn,error) {
    if u.OAuthExchange=="" {return r.conn(u),nil}
    token,err:=identity.Exchange(ctx,cfg.OAuth[u.OAuthExchange],u.OAuthAudience,u.OAuthScopes)
    if err!=nil {return nil,err}
    return mcp.NewConnWithToken(u.URL,token,time.Duration(u.TimeoutMS)*time.Millisecond),nil
}
func (r *Runtime) conn(u config.Upstream) *mcp.Conn {
	key := u.URL + "/" + u.AuthEnv
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.connections[key]
	if c == nil {
		c = mcp.NewConn(u.URL, u.AuthEnv, time.Duration(u.TimeoutMS)*time.Millisecond)
		r.connections[key] = c
	}
	return c
}
func (r *Runtime) Discover(ctx context.Context, cfg *config.Config, upstream string) ([]map[string]any, error) {
	u, ok := cfg.Upstreams[upstream]
	if !ok {
		return nil, fmt.Errorf("unknown upstream %s", upstream)
	}
	if err := checkOutbound(u.URL); err != nil {
		return nil, err
	}
	conn,err:=r.connFor(ctx,cfg,u)
    if err!=nil {return nil,err}
    return conn.ListTools(ctx)
}

// DiscoverCatalog retrieves the full advertised MCP catalog under one SDK
// session. Its results are untrusted diagnostics and are never auto-exposed.
func (r *Runtime) DiscoverCatalog(ctx context.Context, cfg *config.Config, upstream string) (mcp.Catalog,error) {
    u,ok:=cfg.Upstreams[upstream]
    if !ok {return mcp.Catalog{},fmt.Errorf("unknown upstream %s",upstream)}
    if err:=checkOutbound(u.URL);err!=nil {return mcp.Catalog{},err}
    conn,err:=r.connFor(ctx,cfg,u)
    if err!=nil {return mcp.Catalog{},err}
    return conn.DiscoverCatalog(ctx)
}
func checkOutbound(raw string) error { return netguard.ValidateURL(raw) }

func httpClient(timeout time.Duration) *http.Client { return netguard.Client(timeout) }

type Outcome struct {
	RunID    string         `json:"run_id"`
	Revision string         `json:"revision"`
	Status   string         `json:"status"`
	Value    any            `json:"value,omitempty"`
	MCP      map[string]any `json:"mcp,omitempty"`
	Error    string         `json:"error,omitempty"`
	Nodes    map[string]any `json:"nodes,omitempty"`
}
type Checkpoint struct {
	RunID       string            `json:"run_id"`
	Revision    string            `json:"revision"`
	Surface     string            `json:"surface"`
	Tool        string            `json:"tool"`
	Input       map[string]any    `json:"input"`
	States      map[string]string `json:"states"`
	Values      map[string]any    `json:"values"`
	Fired       map[string]bool   `json:"fired"`
	SuspendNode string            `json:"suspend_node"`
}
type nodeResult struct {
	name     string
	value    any
	mcp      map[string]any
	activate []string
	skip     bool
	suspend  bool
	error    error
}

var ErrDenied = errors.New("policy denied")

func (r *Runtime) Run(ctx context.Context, revision string, cfg *config.Config, surface, toolName string, args map[string]any, dry bool) (Outcome, error) {
	out := Outcome{RunID: storage.ID(), Revision: revision, Status: "running"}
	s, ok := cfg.Surfaces[surface]
	if !ok {
		return out, fmt.Errorf("surface not found")
	}
	var tool config.Tool
	found := false
	for _, t := range s.Tools {
		if t.Name == toolName {
			tool = t
			found = true
			break
		}
	}
	if !found {
		return out, fmt.Errorf("tool not found")
	}
    if err:=enforceDataTrust(cfg,surface,tool);err!=nil {
        out.Status="denied"
        out.Error="untrusted destination"
        if r.Store!=nil {r.Store.WriteTrace(storage.Trace{RunID:out.RunID,Revision:revision,Surface:surface,Tool:toolName,Status:"denied",Message:out.Error})}
        return out,err
    }
	if args == nil {
		args = map[string]any{}
	}
	toolContract := cfg.ToolContracts[surface][toolName]
	if err := toolContract.Input.Validate(args); err != nil {
		return out, fmt.Errorf("tool %s input: %w", toolName, err)
	}
	start := time.Now()
	finish := func(status string, err error) {
		out.Status = status
		if err != nil {
			out.Error = expr.RedactDiagnostic(err.Error())
            if cfg.Surfaces[surface].OAuth!="" {out.Error="operation failed"}
		}
		if err != nil {
			// Error observers are best-effort and cannot turn a rejected effect into success.
			// Mask obvious PII before delivering diagnostics to an external hook.
			_, _ = r.intercept(ctx, cfg, "on_error", surface, toolName, map[string]any{"status": status, "message": out.Error, "run_id": out.RunID}, dry)
		}
		if r.Store != nil {
			r.Store.WriteTrace(storage.Trace{RunID: out.RunID, Revision: revision, Surface: surface, Tool: toolName, Status: status, Message: out.Error, DurationMS: time.Since(start).Milliseconds()})
		}
	}
	var val any = args
	var err error
	val, err = r.intercept(ctx, cfg, "on_request", surface, toolName, val, dry)
	if err != nil {
		finish("denied", err)
		return out, err
	}
	safeArgs, ok := val.(map[string]any)
	if !ok {
		err = fmt.Errorf("on_request must produce an object")
		finish("failure", err)
		return out, err
	}
	if err = toolContract.Input.Validate(safeArgs); err != nil {
		finish("failure", err)
		return out, fmt.Errorf("tool %s transformed input: %w", toolName, err)
	}
	if tool.Pipeline == "" {
		if dry {
			out.Value = map[string]any{"mock": true}
			out.Status = "success"
			return out, nil
		}
		upstream, remote, _ := strings.Cut(tool.Source, ".")
		u := cfg.Upstreams[upstream]
		if err = checkOutbound(u.URL); err == nil {
			if val, err = r.intercept(ctx, cfg, "before_call", surface, toolName, safeArgs, dry); err == nil {
				if mapped, ok := val.(map[string]any); ok {
					safeArgs = mapped
				} else {
					err = fmt.Errorf("invalid interceptor value")
				}
			}
		}
		if err == nil {
			effect := "write"
			if tool.ReadOnly {
				effect = "read"
			}
			raw, e := r.mcpCall(ctx, cfg, u, remote, safeArgs, out.RunID, "proxy", effect)
			err = e
			out.MCP = raw
			out.Value = raw["structuredContent"]
			if out.Value == nil {
				out.Value = raw
			}
			if err == nil {
				before, _ := json.Marshal(out.Value)
				out.Value, err = r.intercept(ctx, cfg, "after_call", surface, toolName, out.Value, dry)
				after, _ := json.Marshal(out.Value)
				if !bytes.Equal(before, after) {
					out.MCP = nil
				}
			}
		}
	} else {
		pipeline := cfg.Pipelines[tool.Pipeline]
		runctx, cancel := context.WithTimeout(ctx, time.Duration(pipeline.TimeoutMS)*time.Millisecond)
		defer cancel()
		out, err = r.execute(runctx, revision, cfg, pipeline, out.RunID, surface, toolName, safeArgs, dry, nil)
	}
	if err == nil {
		err = toolContract.Output.Validate(out.Value)
	}
	if err == nil {
		before, _ := json.Marshal(out.Value)
		out.Value, err = r.intercept(ctx, cfg, "before_response", surface, toolName, out.Value, dry)
		after, _ := json.Marshal(out.Value)
		if !bytes.Equal(before, after) {
			out.MCP = nil
		}
		if err == nil {
			err = toolContract.Output.Validate(out.Value)
		}
		if err == nil {
			finish("success", nil)
			out.Status = "success"
			return out, nil
		}
	}
	if errors.Is(err, ErrDenied) {
		finish("denied", err)
	} else if out.Status == "suspended" {
		finish("suspended", err)
	} else {
		finish("failure", err)
	}
	out.Error = expr.RedactDiagnostic(err.Error())
            if cfg.Surfaces[surface].OAuth!="" {out.Error="operation failed"}
	return out, err
}
func (r *Runtime) intercept(ctx context.Context, cfg *config.Config, stage, surface, tool string, value any, dry bool) (any, error) {
	for _, i := range cfg.Interceptors {
		if i.Stage != stage || (i.Surface != "" && i.Surface != surface) || (i.Tool != "" && i.Tool != tool) {
			continue
		}
		v, e := r.runHook(ctx, cfg.Hooks[i.Hook], cfg, value, dry)
		if e != nil {
			return nil, fmt.Errorf("interceptor %s: %w", i.Name, e)
		}
		value = v
	}
	return value, nil
}
func (r *Runtime) execute(ctx context.Context, revision string, cfg *config.Config, p config.Pipeline, runID, surface, toolName string, input map[string]any, dry bool, checkpoint *Checkpoint) (Outcome, error) {
	out := Outcome{RunID: runID, Revision: revision, Status: "running", Nodes: map[string]any{}}
	states := map[string]string{}
	values := map[string]any{}
	fired := map[string]bool{}
	if checkpoint != nil {
		states = checkpoint.States
		values = checkpoint.Values
		fired = checkpoint.Fired
	}
	for name := range p.Nodes {
		if states[name] == "" {
			states[name] = "pending"
		}
	}
	incoming := map[string][]config.Edge{}
	outgoing := map[string][]config.Edge{}
	for _, e := range p.Edges {
		incoming[e.To] = append(incoming[e.To], e)
		outgoing[e.From] = append(outgoing[e.From], e)
	}
	for {
		if e := ctx.Err(); e != nil {
			return out, e
		}
		ready := []string{}
		skippedChanged := false
		for name := range p.Nodes {
			if states[name] != "pending" {
				continue
			}
			if name == p.Entry {
				ready = append(ready, name)
				continue
			}
			deps := incoming[name]
			settled := true
			active := false
			for _, e := range deps {
				if states[e.From] == "pending" {
					settled = false
					break
				}
				if fired[e.From+">"+e.To] {
					active = true
				}
			}
			if !settled {
				n := p.Nodes[name]
				if n.Kind == "join" {
					mode := n.Str("mode")
					count := 0
					for _, dep := range deps {
						if fired[dep.From+">"+dep.To] && states[dep.From] == "completed" {
							count++
						}
					}
					if (mode == "any" && count >= 1) || (mode == "quorum" && count >= n.Int("quorum", 1)) {
						settled = true
					}
				}
				if !settled {
					continue
				}
			}
			if !active {
				states[name] = "skipped"
				skippedChanged = true
				continue
			}
			ready = append(ready, name)
		}
		if len(ready) == 0 {
			if skippedChanged {
				continue
			}
			pending := false
			for _, s := range states {
				if s == "pending" {
					pending = true
				}
			}
			if pending {
				return out, fmt.Errorf("pipeline %s deadlock", p.Name)
			}
			if out.Status == "running" {
				out.Status = "success"
			}
			return out, nil
		}
		sort.Strings(ready)
		// Snapshot: simultaneous nodes cannot observe each other's partial results.
		snap := map[string]any{}
		for k, v := range values {
			snap[k] = v
		}
		results := make([]nodeResult, len(ready))
		sem := make(chan struct{}, p.Concurrency)
		var wg sync.WaitGroup
		for index, name := range ready {
			wg.Add(1)
			go func(i int, name string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results[i] = nodeResult{name: name, error: ctx.Err()}
					return
				}
				vars := map[string]any{"input": input, "nodes": snap}
				results[i] = r.evalNode(ctx, cfg, p.Nodes[name], cfg.Plans[p.Name].Nodes[name], vars, dry, runID, surface, toolName)
			}(index, name)
		}
		wg.Wait()
		// Deterministic commit order even though execution is parallel.
		for _, res := range results {
			if res.error != nil {
				return out, fmt.Errorf("node %s: %w", res.name, res.error)
			}
			if res.suspend {
				cp := Checkpoint{RunID: runID, Revision: revision, Surface: surface, Tool: toolName, Input: input, States: states, Values: values, Fired: fired, SuspendNode: res.name}
				if !dry && r.Store != nil {
					if e := r.Store.PutSuspended(runID, cp); e != nil {
						return out, e
					}
				}
				out.Status = "suspended"
				out.Value = map[string]any{"run_id": runID, "node": res.name}
				return out, fmt.Errorf("execution suspended at %s", res.name)
			}
			states[res.name] = "completed"
			values[res.name] = map[string]any{"value": res.value, "structured": structured(res.mcp), "mcp": res.mcp, "status": "success"}
			if p.Nodes[res.name].Kind == "output" {
				out.Value = res.value
				if res.mcp != nil {
					out.MCP = res.mcp
				}
			}
			for _, e := range outgoing[res.name] {
				should := e.When == "" || e.When == "always" || e.When == fmt.Sprint(res.value)
				if should {
					fired[e.From+">"+e.To] = true
				}
			}
			if r.Store != nil && !dry {
				r.Store.WriteTrace(storage.Trace{RunID: runID, Revision: revision, Surface: surface, Tool: toolName, Node: res.name, Status: "success"})
			}
		}
	}
}
func structured(v map[string]any) any {
	if v == nil {
		return nil
	}
	return v["structuredContent"]
}
func (r *Runtime) evalNode(ctx context.Context, cfg *config.Config, n config.Node, plan *config.NodeIR, vars map[string]any, dry bool, runID, surface, toolName string) nodeResult {
	ret := nodeResult{name: n.Name}
	checkInput := func(value any) bool {
		if err := plan.InputSchema.Validate(value); err != nil {
			ret.error = fmt.Errorf("node %s input: %w", n.Name, err)
			return false
		}
		return true
	}
	get := func(key string) (any, error) {
		compiled, ok := plan.Expressions[key]
		if !ok {
			return nil, fmt.Errorf("node %s: expression %s not compiled", n.Name, key)
		}
		return compiled.Eval(ctx, vars)
	}
	switch n.Kind {
	case "input":
		ret.value = vars["input"]
		checkInput(ret.value)
	case "output", "transform", "filter", "parallel", "join":
		if n.Attrs["value"] != nil {
			ret.value, ret.error = get("value")
		} else if n.Attrs["mapping"] != nil {
			ret.value, ret.error = get("mapping")
		} else {
			ret.value = vars["input"]
		}
		if n.Kind == "filter" && ret.error == nil {
			if fields, ok := n.Attrs["fields"].([]any); ok {
				src, _ := ret.value.(map[string]any)
				out := map[string]any{}
				for _, field := range fields {
					f, _ := field.(string)
					if v, ok := src[f]; ok {
						out[f] = v
					}
				}
				ret.value = out
			}
		}
	case "mask.pii":
		v, e := get("value")
		if e != nil {
			ret.error = e
		} else {
			ret.value = expr.MaskPII(v)
		}
	case "validate":
		v, e := get("value")
		if e != nil {
			ret.error = e
			break
		}
		ret.value = v
		if !checkInput(v) {
			break
		}
		required, _ := n.Attrs["required"].([]any)
		obj, ok := v.(map[string]any)
		if !ok {
			ret.error = fmt.Errorf("value must be object")
			break
		}
		for _, field := range required {
			f, _ := field.(string)
			if _, ok := obj[f]; !ok {
				ret.error = fmt.Errorf("required field %q missing", f)
				break
			}
		}
	case "condition":
		v, e := get("test")
		if e != nil {
			ret.error = e
		} else {
			b, ok := v.(bool)
			if !ok {
				ret.error = fmt.Errorf("condition requires bool")
			} else {
				ret.value = b
			}
		}
	case "suspend":
		ret.suspend = !dry
		ret.value = map[string]any{"suspended": true}
	case "policy.check":
		policy := cfg.Policies[n.Str("policy")]
		if policy.DecisionHook == "" {
			ret.value = true
			break
		}
		v, e := get("value")
		if e != nil {
			ret.error = e
			break
		}
		if !checkInput(v) {
			break
		}
		ret.value, ret.error = r.runHook(ctx, cfg.Hooks[policy.DecisionHook], cfg, v, dry)
		if ret.error == nil {
			ret.value = true
		}
	case "hook.call":
		v, e := get("value")
		if e != nil {
			ret.error = e
			break
		}
		// External hooks participate in the same mandatory interception points
		// as upstream calls. Built-in transforms stay local.
		h := cfg.Hooks[n.Str("hook")]
		if h.Transport != "builtin" {
			v, e = r.intercept(ctx, cfg, "before_call", surface, toolName, v, dry)
			if e != nil {
				ret.error = e
				break
			}
		}
		if !checkInput(v) {
			break
		}
		ret.value, ret.error = r.runHook(ctx, h, cfg, v, dry)
		if ret.error == nil && h.Transport != "builtin" {
			ret.value, ret.error = r.intercept(ctx, cfg, "after_call", surface, toolName, ret.value, dry)
		}
	case "mcp.call":
		v, e := get("arguments")
		if e != nil {
			ret.error = e
			break
		}
		args, ok := v.(map[string]any)
		if !ok {
			ret.error = fmt.Errorf("mcp arguments must be object")
			break
		}
		if !checkInput(args) {
			break
		}
		if dry {
			if mock, ok := n.Attrs["mock"]; ok {
				ret.value = mock
			} else {
				ret.error = fmt.Errorf("node %s has no mock for dry-run", n.Name)
			}
			break
		}
		v, e = r.intercept(ctx, cfg, "before_call", surface, toolName, args, dry)
		if e != nil {
			ret.error = e
			break
		}
		args, ok = v.(map[string]any)
		if !ok {
			ret.error = fmt.Errorf("before_call must return object for mcp.call")
			break
		}
		if !checkInput(args) {
			break
		}
		u := cfg.Upstreams[n.Str("upstream")]
		kind := n.Str("effect")
		if kind == "" {
			kind = "unknown"
		}
		ret.mcp, ret.error = r.mcpCall(ctx, cfg, u, n.Str("tool"), args, runID, n.Name, kind)
		if ret.error == nil {
			ret.value = ret.mcp["structuredContent"]
			if ret.value == nil {
				ret.value = ret.mcp
			}
			before, _ := json.Marshal(ret.value)
			ret.value, ret.error = r.intercept(ctx, cfg, "after_call", surface, toolName, ret.value, dry)
			after, _ := json.Marshal(ret.value)
			if !bytes.Equal(before, after) {
				ret.mcp = nil
			}
		}
	case "http.call":
		v, e := get("value")
		if e != nil {
			ret.error = e
			break
		}
		v, e = r.intercept(ctx, cfg, "before_call", surface, toolName, v, dry)
		if e != nil {
			ret.error = e
			break
		}
		if !checkInput(v) {
			break
		}
		h := config.Hook{URL: n.Str("url"), Transport: "http", Contract: "transform", TimeoutMS: n.Int("timeout_ms", 10000), AuthEnv: n.Str("auth_env")}
		ret.value, ret.error = r.runHook(ctx, h, cfg, v, dry)
		if ret.error == nil {
			ret.value, ret.error = r.intercept(ctx, cfg, "after_call", surface, toolName, ret.value, dry)
		}
	default:
		ret.error = fmt.Errorf("unsupported node kind %s", n.Kind)
	}
	if ret.error == nil && !ret.suspend {
		if err := plan.OutputSchema.Validate(ret.value); err != nil {
			ret.error = fmt.Errorf("node %s output: %w", n.Name, err)
		}
	}
	return ret
}
func (r *Runtime) mcpCall(ctx context.Context,cfg *config.Config,u config.Upstream,tool string,args map[string]any,runID,node,effect string)(map[string]any,error) {
    if err:=checkOutbound(u.URL);err!=nil {return nil,err}
    // Acquire and verify the target token before any dispatching effect is
    // journaled. Authorization failure means no external write was attempted.
    conn,err:=r.connFor(ctx,cfg,u)
    if err!=nil{return nil,err}
    id:=storage.ID()
	if effect != "read" && r.Store != nil {
		if e := r.Store.Effect(storage.Effect{ID: id, RunID: runID, Node: node, Operation: u.Name + "." + tool, Status: "dispatching"}); e != nil {
			return nil, e
		}
	}
	raw, e := conn.CallTool(ctx, tool, args)
	if effect != "read" && r.Store != nil {
		state := "confirmed"
		reason := ""
		if e != nil {
			state = "unknown"
			reason = expr.RedactDiagnostic(e.Error())
		}
		if ee := r.Store.Effect(storage.Effect{ID: id, RunID: runID, Node: node, Operation: u.Name + "." + tool, Status: state, Reason: reason}); ee != nil {
			return nil, ee
		}
	}
	return raw, e
}
func (r *Runtime) runHook(ctx context.Context, h config.Hook, cfg *config.Config, value any, dry bool) (any, error) {
	if dry {
		return value, nil
	}
	switch h.Transport {
	case "builtin":
		if h.Name == "mask_pii" || h.Contract == "transform" {
			return expr.MaskPII(value), nil
		}
		return value, nil
	case "mcp":
		u := cfg.Upstreams[h.Upstream]
		args, ok := value.(map[string]any)
		if !ok {
			args = map[string]any{"input": value}
		}
		raw, e := r.mcpCall(ctx, cfg, u, h.Tool, args, storage.ID(), "hook:"+h.Name, "unknown")
		if e != nil {
			return nil, e
		}
		if h.Contract == "decision" {
			return decide(raw)
		}
		if v, ok := raw["structuredContent"]; ok {
			return v, nil
		}
		return raw, nil
	case "http":
		if e := checkOutbound(h.URL); e != nil {
			return nil, e
		}
		body, _ := json.Marshal(map[string]any{"input": value})
		timeout := time.Duration(h.TimeoutMS) * time.Millisecond
		hc := httpClient(timeout)
		request, e := http.NewRequestWithContext(ctx, "POST", h.URL, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		request.Header.Set("Content-Type", "application/json")
		if h.OAuthExchange!="" {
            token,err:=identity.Exchange(ctx,cfg.OAuth[h.OAuthExchange],h.OAuthAudience,h.OAuthScopes)
            if err!=nil{return nil,err}
            request.Header.Set("Authorization","Bearer "+token)
        } else if h.AuthEnv != "" {
			if token := os.Getenv(h.AuthEnv); token != "" {
				request.Header.Set("Authorization", "Bearer "+token)
			}
		}
		response, e := hc.Do(request)
		if e != nil {
			return nil, e
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, fmt.Errorf("hook %s: HTTP %d", h.Name, response.StatusCode)
		}
		raw, e := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		if e != nil {
			return nil, e
		}
		var result map[string]any
		if e = json.Unmarshal(raw, &result); e != nil {
			return nil, e
		}
		if h.Contract == "decision" {
			return decide(result)
		}
		if act, _ := result["action"].(string); act == "deny" {
			return nil, ErrDenied
		}
		if v, ok := result["value"]; ok {
			return v, nil
		}
		return result, nil
	}
	return nil, fmt.Errorf("unknown hook transport %s", h.Transport)
}
func decide(value any) (any, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid decision response")
	}
	// Both {decision:"allow"} and {structuredContent:{decision:"allow"}}.
	if nested, ok := m["structuredContent"].(map[string]any); ok {
		m = nested
	}
	if v, ok := m["decision"].(string); ok && v == "allow" {
		if obligations, present := m["obligations"]; present {
			if items, yes := obligations.([]any); !yes || len(items) > 0 {
				return nil, fmt.Errorf("unenforced decision obligations; deny")
			}
		}
		return true, nil
	}
	return nil, ErrDenied
}
func (r *Runtime) Resume(ctx context.Context, cfg *config.Config, revision string, runID string, value any) (Outcome, error) {
	var cp Checkpoint
	if e := r.Store.GetSuspended(runID, &cp); e != nil {
		return Outcome{}, e
	}
	if cp.Revision != revision {
		return Outcome{}, fmt.Errorf("resume requires revision %s, received %s", cp.Revision, revision)
	}
	s := cfg.Surfaces[cp.Surface]
	var pipeline string
    var selected config.Tool
	for _, t := range s.Tools {
		if t.Name == cp.Tool {
			pipeline = t.Pipeline
            selected=t
			break
		}
	}
    if err:=enforceDataTrust(cfg,cp.Surface,selected);err!=nil {return Outcome{RunID:runID,Revision:revision,Status:"denied",Error:"untrusted destination"},err}
	if pipeline == "" {
		return Outcome{}, fmt.Errorf("missing original tool")
	}
	cp.States[cp.SuspendNode] = "completed"
	cp.Values[cp.SuspendNode] = map[string]any{"value": value, "status": "success"}
	for _, edge := range cfg.Pipelines[pipeline].Edges {
		if edge.From == cp.SuspendNode && (edge.When == "" || edge.When == "always") {
			cp.Fired[edge.From+">"+edge.To] = true
		}
	}
	out, e := r.execute(ctx, revision, cfg, cfg.Pipelines[pipeline], runID, cp.Surface, cp.Tool, cp.Input, false, &cp)
	if e == nil {
		_ = r.Store.DeleteSuspended(runID)
	}
	return out, e
}

// ReadResource forwards an MCP resources/read request, preserving the upstream content shape.
func (r *Runtime) ReadResource(ctx context.Context,cfg *config.Config,u config.Upstream,uri string)(map[string]any,error) {
    if e:=checkOutbound(u.URL);e!=nil{return nil,e}
    conn,err:=r.connFor(ctx,cfg,u)
    if err!=nil {return nil,err}
    raw, e := conn.Call(ctx, "resources/read", map[string]any{"uri": uri})
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if e = json.Unmarshal(raw, &v); e != nil {
		return nil, e
	}
	return v, nil
}
