package engine

import (
	"bytes"
	"context"
    "crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/expr"
	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/netguard"
	"github.com/burkostya/regate/internal/storage"
    "github.com/burkostya/regate/internal/strictjson"
)

// effectScope binds external writes (including HTTP hooks) to a stable
// logical run and step. Nested interceptors have their own step identity;
// no credentials or sensitive payloads are stored in this context.
type effectScope struct {
    runID string
    node string
}
type effectScopeKey struct{}

func withEffectScope(ctx context.Context, runID string) context.Context {
    return context.WithValue(ctx, effectScopeKey{}, effectScope{runID:runID,node:"run"})
}
func withEffectStep(ctx context.Context, step string) context.Context {
    scope,ok:=ctx.Value(effectScopeKey{}).(effectScope)
    if !ok || scope.runID=="" {return ctx}
    scope.node += "/"+step
    return context.WithValue(ctx,effectScopeKey{},scope)
}
func requireEffectScope(ctx context.Context)(effectScope,error) {
    scope,ok:=ctx.Value(effectScopeKey{}).(effectScope)
    if !ok || scope.runID=="" || scope.node=="" {
        return effectScope{},fmt.Errorf("durable effect identity required for external hook")
    }
    return scope,nil
}

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
    if err:=r.CheckQueuedTaskExecution(ctx);err!=nil{return nil,err}
    if err:=r.CheckGrantExecution(ctx);err!=nil{return nil,err}
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

// Payloads may grow after CEL transformations; bound outbound hook data
// before journaling or dispatch, independently of the inbound MCP body cap.
const maxHTTPHookRequestBytes = 4 << 20

func httpClient(timeout time.Duration) *http.Client { return netguard.Client(timeout) }

type Outcome struct {
	RunID    string         `json:"run_id"`
	Revision string         `json:"revision"`
	Status   string         `json:"status"`
	Value    any            `json:"value,omitempty"`
	MCP      map[string]any `json:"mcp,omitempty"`
	Error    string         `json:"error,omitempty"`
	Nodes    map[string]any `json:"nodes,omitempty"`
	OutputProduced bool `json:"-"` // True even for a deliberately null output.
	NamedOutputs map[string]any `json:"-"` // Private, completed child terminal values; never client-facing.
	Checkpoint *Checkpoint `json:"-"` // Private continuation state, never sent to MCP clients.
	SuspensionPaths [][]string `json:"-"` // Public-safe node identities for task continuation selection.
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
	Child       *Checkpoint       `json:"child,omitempty"` // Legacy single active suspension.
	Suspensions map[string]*Checkpoint `json:"suspensions,omitempty"` // Multiple independently parked nodes, each with its own nested continuation.
	FrameKind string `json:"frame_kind,omitempty"` // Only "map" for a durable per-item wrapper, not a pipeline snapshot.
	MapIndex int `json:"map_index,omitempty"`
	MapItems []any `json:"map_items,omitempty"` // Original frozen input list, not recomputed on Resume.
	MapResults []any `json:"map_results,omitempty"` // Completed immutable prefix, never replayed.
}

func derivedSubrunID(parentRunID, nodeName, childName string) string {
	sum := sha256.Sum256([]byte(parentRunID + "\x00" + nodeName + "\x00" + childName))
	return fmt.Sprintf("%s/sub-%x", parentRunID, sum[:8])
}

func mappedIterationRunID(parentRunID, nodeName, childName string, index int) string {
	return derivedSubrunID(parentRunID, nodeName, fmt.Sprintf("map:%s:%d", childName, index))
}

func mappedFrameRunID(parentRunID, nodeName, childName string) string {
	return derivedSubrunID(parentRunID, nodeName, "map-frame:"+childName)
}
type dataPortBinding struct {
	producer string
	fromPort string
}

type nodeResult struct {
	name     string
	value    any
	mcp      map[string]any
	activate []string
	skip     bool
	suspend  bool
	checkpoint *Checkpoint
	error    error
}

var ErrDenied = errors.New("policy denied")

// scrubPublishedResult clears all partial client-facing values. A pending
// suspension is not a failed result and must retain its checkpoint separately.
func scrubPublishedResult(out *Outcome) {
	out.Value = nil
	out.MCP = nil
	out.NamedOutputs = nil
	out.OutputProduced = false
	out.Nodes = nil
	out.Checkpoint = nil
}

// A published MCP pipeline tool must produce exactly one response terminal,
// even when its control graph has conditional outputs. Zero completed outputs
// is a missing response, not an implicit successful JSON null. More than one
// successful terminal must not select whichever parallel node finished last.
// Called pipelines with explicit multi-terminal contracts aggregate separately.
func requireUniqueSurfaceOutput(out *Outcome, pipelineName string) error {
	if len(out.NamedOutputs) == 1 && out.OutputProduced {
		return nil
	}
	if len(out.NamedOutputs) == 0 {
		// A completed output with an actual null value *does* have a
		// NamedOutputs key. No such key means no root result at all.
		scrubPublishedResult(out)
		out.Status = "failure"
		return fmt.Errorf("surface pipeline %s produced no output", pipelineName)
	}
	names := make([]string, 0, len(out.NamedOutputs))
	for name := range out.NamedOutputs {
		names = append(names, name)
	}
	sort.Strings(names)
	// No partial result or MCP envelope may escape on ambiguous success.
	scrubPublishedResult(out)
	out.Status = "failure"
	return fmt.Errorf("surface pipeline %s has competing outputs: %s", pipelineName, strings.Join(names, ", "))
}

// finalizePublishedOutput is the same release boundary for initial and
// resumed tools. A suspended run never invokes response interceptors; only a
// fully settled result is eligible. A failing schema or response hook clears
// all client-visible result fields, including a previously returned MCP
// envelope, so callers cannot observe partially verified data.
func (r *Runtime) finalizePublishedOutput(ctx context.Context, cfg *config.Config, surface, toolName string,
	contract *config.SchemaContract, out *Outcome, dry bool) error {
	scrub := func() { scrubPublishedResult(out) }
	if err := contract.Validate(out.Value); err != nil {
		scrub()
		return err
	}
	before, _ := json.Marshal(out.Value)
	value, err := r.intercept(ctx, cfg, "before_response", surface, toolName, out.Value, dry)
	if err != nil {
		scrub()
		return err
	}
	out.Value = value
	after, _ := json.Marshal(out.Value)
	if !bytes.Equal(before, after) {
		out.MCP = nil
	}
	if err := contract.Validate(out.Value); err != nil {
		scrub()
		return err
	}
	return nil
}

func (r *Runtime) Run(ctx context.Context, revision string, cfg *config.Config, surface, toolName string, args map[string]any, dry bool) (Outcome, error) {
	out := Outcome{RunID: storage.ID(), Revision: revision, Status: "running"}
    if err:=r.CheckQueuedTaskExecution(ctx);err!=nil{return out,err}
    ctx=withEffectScope(ctx,out.RunID)
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
    if r.Store!=nil {
        if err:=r.Store.RequireExecutionLeader(ctx);err!=nil{return out,err}
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
		if out.Status == "suspended" && (dry || r.Store == nil || out.Checkpoint == nil) {
			out.Status = "failure"
			err = fmt.Errorf("durable pipeline checkpoint unavailable")
		}
		if err == nil {
			err = requireUniqueSurfaceOutput(&out, pipeline.Name)
		}
	}
	if err == nil {
		err = r.finalizePublishedOutput(ctx, cfg, surface, toolName, toolContract.Output, &out, dry)
	}
	if err == nil {
		finish("success", nil)
		out.Status = "success"
		return out, nil
	}
	if errors.Is(err, ErrDenied) {
		finish("denied", err)
	} else if out.Status == "suspended" {
		finish("suspended", err)
	} else {
		finish("failure", err)
	}
	if out.Status != "suspended" {
		scrubPublishedResult(&out)
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
		v, e := r.runHook(withEffectStep(ctx,"interceptor:"+stage+":"+i.Name), cfg.Hooks[i.Hook], cfg, value, dry)
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
		if checkpoint.RunID != runID || checkpoint.Revision != revision ||
			checkpoint.Surface != surface || checkpoint.Tool != toolName ||
			checkpoint.Input == nil || checkpoint.States == nil ||
			checkpoint.Values == nil || checkpoint.Fired == nil {
			return out, fmt.Errorf("nested checkpoint identity or state mismatch")
		}
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
	dataInputs := map[string]map[string]dataPortBinding{}
	for _, e := range p.Edges {
		if e.Kind == "data" {
			if dataInputs[e.To] == nil { dataInputs[e.To] = map[string]dataPortBinding{} }
			fromPort := e.FromPort
			if fromPort == "" { fromPort = "value" }
			dataInputs[e.To][e.ToPort] = dataPortBinding{producer:e.From, fromPort:fromPort}
			continue
		}
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
			firedCount := 0
			for _, edge := range deps {
				if states[edge.From] == "pending" {
					settled = false
					break
				}
				if states[edge.From] == "completed" && fired[edge.From+">"+edge.To] {
					firedCount++
				}
			}
			// Every join is a full settlement barrier. Running an any/quorum
			// join before other predecessors settle can cause a downstream
			// write to race a later denial, suspension or unknown effect.
			// All/any/quorum affect the activation threshold, NOT permission
			// to speculate past still-pending incoming branches.
			if !settled {
				continue
			}
			threshold := 1 // Ordinary graph merges accept any active input.
			if node := p.Nodes[name]; node.Kind == "join" {
				switch node.Str("mode") {
				case "", "all":
					threshold = len(deps)
				case "any":
					threshold = 1
				case "quorum":
					threshold = node.Int("quorum", 0)
				default:
					return out, fmt.Errorf("pipeline %s join %s has unsupported mode",p.Name,name)
				}
				if threshold < 1 || threshold > len(deps) {
					return out, fmt.Errorf("pipeline %s join %s has invalid threshold",p.Name,name)
				}
			}
			if firedCount < threshold {
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
			// Reconstruct completed output terminals from durable values, not
			// only from the current wave: resumed child frames may have
			// produced a sibling output before suspension.
			out.NamedOutputs = map[string]any{}
			for name, node := range p.Nodes {
				if node.Kind != "output" || states[name] != "completed" { continue }
				state, present := values[name].(map[string]any)
				if !present {
					return out, fmt.Errorf("pipeline %s terminal %s has no completed value record", p.Name, name)
				}
				value, present := state["value"]
				if !present {
					return out, fmt.Errorf("pipeline %s terminal %s has no completed output value", p.Name, name)
				}
				out.NamedOutputs[name] = value
			}
			// A root output may have completed in a wave before a sibling
			// suspension. On resumed execution it is absent from this wave's
			// nodeResult list, but it is still the single published terminal.
			// Reconstruct the public response only after every branch settles;
			// the shared final response boundary then validates/redacts it.
			// A completed child may have emitted *multiple* output terminals
			// before a sibling suspended. Resume reconstructs NamedOutputs
			// from the checkpoint, even if this wave executes no output node.
			// Presence is distinct from the single-value root response below.
			out.OutputProduced = len(out.NamedOutputs) > 0
			if len(out.NamedOutputs) == 1 {
				for name, value := range out.NamedOutputs {
					out.Value = value
					out.OutputProduced = true // A completed JSON null is also present.
					record := values[name].(map[string]any)
					out.MCP, _ = record["mcp"].(map[string]any)
				}
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
				// Bind only authenticated, completed producer values. Validation
				// precedes the consumer's external effect or interceptor.
				ports := map[string]any{}
				present := map[string]bool{}
				for port, binding := range dataInputs[name] {
					if cfg.Plans[p.Name].Nodes[name].OptionalPorts[port] &&
						states[binding.producer] == "skipped" {
						ports[port] = nil
						present[port] = false
						continue
					}
					if states[binding.producer] != "completed" {
						results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s producer has not completed", port)}
						return
					}
					state, ok := snap[binding.producer].(map[string]any)
					if !ok {
						results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s unavailable", port)}
						return
					}
					var value any
					if binding.fromPort != "value" && p.Nodes[binding.producer].Kind == "pipeline.call" {
						object, isObject := state["value"].(map[string]any)
						if isObject { value, ok = object[binding.fromPort] }
						if isObject && !ok &&
							cfg.Plans[p.Name].Nodes[binding.producer].OptionalOutputPorts[binding.fromPort] &&
							cfg.Plans[p.Name].Nodes[name].OptionalPorts[port] {
							ports[port] = nil
							present[port] = false
							continue
						}
						if !isObject || !ok {
							results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s missing named pipeline output %s", port, binding.fromPort)}
							return
						}
					} else if binding.fromPort == "structured" || binding.fromPort == "content" {
						field := "structuredContent"
						if binding.fromPort == "content" { field = "content" }
						raw, hasEnvelope := state["mcp"].(map[string]any)
						if hasEnvelope { value, ok = raw[field] }
						if !hasEnvelope || !ok {
							results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s missing %s MCP output", port, binding.fromPort)}
							return
						}
					} else {
						value, ok = state["value"]
						if !ok {
							results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s has no producer value", port)}
							return
						}
					}
					if err := cfg.Plans[p.Name].Nodes[name].InputPorts[port].Validate(value); err != nil {
						results[i] = nodeResult{name: name, error: fmt.Errorf("data port %s failed schema validation", port)}
						return
					}
					ports[port] = value
					present[port] = true
				}
				vars := map[string]any{"input": input, "nodes": snap, "ports": ports, "port_present": present}
				var childCheckpoint *Checkpoint
				if checkpoint != nil && (p.Nodes[name].Kind == "pipeline.call" || p.Nodes[name].Kind == "map") {
					if checkpoint.Suspensions != nil {
						childCheckpoint = checkpoint.Suspensions[name]
					} else if checkpoint.SuspendNode == name {
						childCheckpoint = checkpoint.Child
					}
				}
				results[i] = r.evalNode(ctx, cfg, p.Nodes[name], cfg.Plans[p.Name].Nodes[name], vars, dry, runID, surface, toolName, revision, childCheckpoint)
			}(index, name)
		}
		wg.Wait()
		// Every ready node has already executed. Inspect all failures before
		// accepting a suspension, then record every successful sibling in the
		// checkpoint. Otherwise an effectful sibling could be dispatched again.
		suspensions := map[string]*Checkpoint{}
		for i := range results {
			res := &results[i]
			if res.error != nil {
				return out, fmt.Errorf("node %s: %w", res.name, res.error)
			}
			if res.suspend {
				suspensions[res.name] = res.checkpoint
			}
		}
		// Deterministic commit order even though execution is parallel.
		for _, res := range results {
			if res.suspend { continue } // Remains pending until the authorized resume.
			states[res.name] = "completed"
			values[res.name] = map[string]any{"value": res.value, "structured": structured(res.mcp), "mcp": res.mcp, "status": "success"}
			if p.Nodes[res.name].Kind == "output" {
				out.OutputProduced = true
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
		if len(suspensions) > 0 {
			cp := &Checkpoint{RunID: runID, Revision: revision, Surface: surface,
				Tool: toolName, Input: input, States: states, Values: values, Fired: fired}
			if len(suspensions) == 1 {
				for name, child := range suspensions {
					cp.SuspendNode, cp.Child = name, child
				}
			} else {
				cp.Suspensions = suspensions
			}
			// Only the root run has a SQLite checkpoint. All parallel siblings
			// have settled, and their completed effects remain in the snapshot.
			if !dry && r.Store != nil && len(runID) == 24 {
				if err := r.Store.PutSuspended(runID, cp); err != nil {
					return out, fmt.Errorf("persist root pipeline checkpoint: %w", err)
				}
			}
			out.Status = "suspended"
			out.Checkpoint = cp
			out.SuspensionPaths = PendingSuspensionPaths(cp)
			out.Value = map[string]any{"run_id": runID, "paths": out.SuspensionPaths}
			if len(suspensions) == 1 { out.Value.(map[string]any)["node"] = cp.SuspendNode }
			return out, fmt.Errorf("execution suspended at %d node(s)", len(suspensions))
		}
	}
}
func structured(v map[string]any) any {
	if v == nil {
		return nil
	}
	return v["structuredContent"]
}
func (r *Runtime) evalNode(ctx context.Context, cfg *config.Config, n config.Node, plan *config.NodeIR, vars map[string]any, dry bool, runID, surface, toolName, revision string, childCheckpoint *Checkpoint) nodeResult {
    ctx=withEffectStep(ctx,"node:"+n.Name)
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
	case "pipeline.call":
		// A resumed child must use the exact arguments durably stored at the
		// first suspension, never reevaluate a possibly changed expression.
		childName := n.Str("pipeline")
		child, ok := cfg.Pipelines[childName]
		if !ok { ret.error = fmt.Errorf("unknown called pipeline"); break }
		nestedRunID := derivedSubrunID(runID, n.Name, childName)
		var args map[string]any
		if childCheckpoint != nil {
			if childCheckpoint.RunID != nestedRunID ||
				childCheckpoint.Revision != revision ||
				childCheckpoint.Surface != surface || childCheckpoint.Tool != toolName {
				ret.error = fmt.Errorf("nested checkpoint does not match called pipeline")
				break
			}
			args = childCheckpoint.Input
		} else {
			value, err := get("arguments")
			if err != nil { ret.error = err; break }
			args, ok = value.(map[string]any)
			if !ok {
				ret.error = fmt.Errorf("pipeline.call arguments must be an object")
				break
			}
		}
		if args == nil { ret.error = fmt.Errorf("pipeline.call arguments must be an object"); break }
		if !checkInput(args) { break }
		nestedContext, cancel := context.WithTimeout(ctx, time.Duration(child.TimeoutMS)*time.Millisecond)
		// Keep MCP, hook and HTTP effect identities on the same derived
		// child scope. This scopes every nested external write to its
		// immutable parent run and call frame.
		nestedContext = withEffectScope(nestedContext, nestedRunID)
		if !dry && r.Store != nil {
			r.Store.WriteTrace(storage.Trace{RunID:runID, Revision:revision, Surface:surface, Tool:toolName,
				Node:n.Name, Status:"subrun", Message:"nested run "+nestedRunID})
		}
		nested, nestedErr := r.execute(nestedContext, revision, cfg, child, nestedRunID, surface, toolName, args, dry, childCheckpoint)
		cancel()
		if nested.Status == "suspended" {
			if nested.Checkpoint == nil || nestedErr == nil {
				ret.error = fmt.Errorf("called pipeline %s returned invalid suspension", childName)
			} else {
				ret.suspend = true
				ret.checkpoint = nested.Checkpoint
			}
			break
		}
		if nestedErr != nil {
			ret.error = fmt.Errorf("called pipeline %s: %w", childName, nestedErr)
			break
		}
		if !nested.OutputProduced {
			ret.error = fmt.Errorf("called pipeline %s did not produce an output", childName)
			break
		}
		terminalNames := []string{}
		for terminalName, node := range child.Nodes {
			if node.Kind == "output" { terminalNames = append(terminalNames, terminalName) }
		}
		if len(terminalNames) > 1 {
			sort.Strings(terminalNames)
			outputs := make(map[string]any, len(terminalNames))
			for _, terminalName := range terminalNames {
				value, present := nested.NamedOutputs[terminalName]
				if !present {
					if plan.OptionalOutputPorts[terminalName] { continue }
					ret.error = fmt.Errorf("called pipeline %s missing required child output %s", childName, terminalName)
					break
				}
				outputs[terminalName] = value
			}
			if ret.error != nil { break }
			ret.value = outputs
		} else {
			ret.value = nested.Value
		}
	case "map":
		// Both modes are bounded and ordered. Pipeline-backed maps can
		// checkpoint one suspended child iteration with a frozen array and
		// immutable prefix of already completed item results.
		childName := n.Str("pipeline")
		child, hasChild := cfg.Pipelines[childName]
		var items []any
		multiOutput := false
		if hasChild {
			terminals := 0
			for _, node := range child.Nodes {
				if node.Kind == "output" { terminals++ }
			}
			multiOutput = terminals > 1
		}
		start := 0
		result := []any{}
		if childCheckpoint != nil {
			if childName == "" || childCheckpoint.FrameKind != "map" ||
				childCheckpoint.RunID != mappedFrameRunID(runID, n.Name, childName) ||
				childCheckpoint.Revision != revision || childCheckpoint.Surface != surface ||
				childCheckpoint.Tool != toolName || childCheckpoint.Child == nil {
				ret.error = fmt.Errorf("invalid durable mapped iteration identity")
				break
			}
			items = childCheckpoint.MapItems
			start = childCheckpoint.MapIndex
			result = append(result, childCheckpoint.MapResults...)
		} else {
			raw, err := get("items")
			if err != nil { ret.error = err; break }
			var ok bool
			items, ok = raw.([]any)
			if !ok { ret.error = fmt.Errorf("map items must be an array"); break }
		}
		maxItems := n.Int("max_items", 0)
		if maxItems < 1 || maxItems > 64 || len(items) > maxItems ||
			start < 0 || start > len(items) || (childCheckpoint != nil &&
			(start >= len(items) || len(result) != start)) {
			ret.error = fmt.Errorf("map item count or restored prefix exceeds configured bound")
			break
		}
		// Opt-in parallelism is limited to effect-free, non-suspendable maps,
		// either CEL or a statically verified transitive-pure child pipeline.
		// Effectful/suspendable children retain sequential per-item journals
		// and durable checkpoint order.
		if childName == "" {
			parallelism := n.Int("parallelism", 1)
			if parallelism < 1 || parallelism > 16 || parallelism > maxItems {
				ret.error = fmt.Errorf("invalid pure map parallelism")
			} else {
				ret.value, ret.error = evaluatePureMap(ctx, plan, items, vars,
                    effectivePureMapParallelism(ctx, parallelism))
			}
			break
		}
		if childName != "" {
			if !hasChild || cfg.Plans[childName] == nil ||
				cfg.Plans[childName].Nodes[child.Entry] == nil {
				ret.error = fmt.Errorf("map child pipeline missing")
				break
			}
			// Preflight the *whole* array even on Resume. No malformed later
			// item can trigger avoidable partial writes, and the restored input
			// cannot override a child's original contract.
			childInput := cfg.Plans[childName].Nodes[child.Entry].InputSchema
			for i, item := range items {
				arguments, ok := item.(map[string]any)
				if !ok || arguments == nil {
					ret.error = fmt.Errorf("map item %d: pipeline argument must be an object", i)
					break
				}
				if err := plan.InputSchema.Validate(item); err != nil {
					ret.error = fmt.Errorf("map item %d failed schema validation", i)
					break
				}
				if err := childInput.Validate(item); err != nil {
					ret.error = fmt.Errorf("map item %d failed child input schema validation", i)
					break
				}
			}
			if ret.error != nil { break }
		}
		if _, declared := n.Attrs["parallelism"]; declared {
			parallelism := n.Int("parallelism", 0)
			if parallelism < 1 || parallelism > 16 || parallelism > maxItems ||
				n.Attrs["allow_effects"] == true || n.Attrs["allow_suspend"] == true ||
				childCheckpoint != nil {
				ret.error = fmt.Errorf("invalid effect-free pipeline map parallelism")
				break
			}
			// Within an outer pure-map worker, keep the declared HCL
            // contract but execute this nested batch sequentially. Nested
            // pools would multiply the live child goroutine count.
            if effectivePureMapParallelism(ctx, parallelism) > 1 {
				ret.value, ret.error = r.evaluatePurePipelineMap(ctx, cfg, child, plan,
					items, parallelism, runID, surface, toolName, revision, dry)
				break
			}
		}
		for i := start; i < len(items); i++ {
			if err := ctx.Err(); err != nil {ret.error = err;break}
			item := items[i]
			if err := plan.InputSchema.Validate(item); err != nil {
				ret.error = fmt.Errorf("map item %d failed schema validation", i)
				break
			}
			arguments, ok := item.(map[string]any)
			if !ok || arguments == nil {
				ret.error = fmt.Errorf("map item %d: pipeline argument must be an object", i)
				break
			}
			subrunID := mappedIterationRunID(runID, n.Name, childName, i)
			childCtx, cancel := context.WithTimeout(ctx, time.Duration(child.TimeoutMS)*time.Millisecond)
			childCtx = withEffectScope(childCtx, subrunID)
			if !dry && r.Store != nil {
				r.Store.WriteTrace(storage.Trace{RunID:runID, Revision:revision, Surface:surface, Tool:toolName,
					Node:fmt.Sprintf("%s[%d]",n.Name,i), Status:"subrun", Message:"nested run "+subrunID})
			}
			var activeChild *Checkpoint
			if childCheckpoint != nil && i == start {
				activeChild = childCheckpoint.Child
			}
			nested, nestedErr := r.execute(childCtx, revision, cfg, child, subrunID,
				surface, toolName, arguments, dry, activeChild)
			cancel()
			if nested.Status == "suspended" {
				if n.Attrs["allow_suspend"] != true || n.Attrs["allow_effects"] != true ||
					nested.Checkpoint == nil || nestedErr == nil {
					ret.error = fmt.Errorf("map item %d: child suspension is not authorized", i)
				} else {
					ret.suspend = true
					ret.checkpoint = &Checkpoint{RunID:mappedFrameRunID(runID,n.Name,childName),
						Revision:revision,Surface:surface,Tool:toolName,FrameKind:"map",
						MapIndex:i,MapItems:items,MapResults:result,Child:nested.Checkpoint}
				}
				break
			}
			if nestedErr != nil {
				ret.error = fmt.Errorf("map item %d: child pipeline failed: %w", i, nestedErr)
				break
			}
			if !nested.OutputProduced {
				ret.error = fmt.Errorf("map item %d: child pipeline produced no output", i)
				break
			}
			var itemOutput any = nested.Value
			if multiOutput {
				// The per-item object contains each completed named terminal.
				// Optional terminals may be absent, never silently null.
				fields := make(map[string]any, len(nested.NamedOutputs))
				for name, value := range nested.NamedOutputs { fields[name] = value }
				itemOutput = fields
			}
			if plan.MapItemSchema == nil || plan.MapItemSchema.Validate(itemOutput) != nil {
				ret.error = fmt.Errorf("map item %d failed typed child output contract", i)
				break
			}
			result = append(result, itemOutput)
		}
		if ret.error == nil && !ret.suspend { ret.value = result }
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
				if structuredMock, provided := n.Attrs["mock_structured"]; provided {
					ret.mcp = map[string]any{"structuredContent": structuredMock}
				}
				if contentMock, provided := n.Attrs["mock_content"]; provided {
					if ret.mcp == nil { ret.mcp = map[string]any{} }
					ret.mcp["content"] = contentMock
				}
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
		callContext := ctx
		if retry, present := n.Attrs["retry"].(map[string]any); present {
			attempts, attemptsOK := retry["max_attempts"].(int64)
			delay, delayOK := retry["delay_ms"].(int64)
			safe, safeOK := retry["safe_read"].(bool)
			if kind != "read" || !attemptsOK || !delayOK || !safeOK || !safe ||
				attempts < 2 || attempts > 4 || delay < 1 || delay > 2000 || len(retry) != 3 {
				ret.error = fmt.Errorf("invalid runtime MCP read retry contract")
				break
			}
			callContext = withReadRetry(ctx, int(attempts), time.Duration(delay)*time.Millisecond)
		}
		ret.mcp, ret.error = r.mcpCall(callContext, cfg, u, n.Str("tool"), args, runID, n.Name, kind)
		if ret.error == nil {
			if err := validateMCPToolOutcome(ret.mcp); err != nil {
				ret.error = err
				ret.mcp = nil
				break
			}
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
	if ret.error == nil && !ret.suspend {
		for portName, contract := range plan.OutputPorts {
			var value any
			if portName == "value" {
				value = ret.value
			} else if n.Kind == "pipeline.call" {
				object, isObject := ret.value.(map[string]any)
				var provided bool
				if isObject { value, provided = object[portName] }
				if isObject && !provided && plan.OptionalOutputPorts[portName] {
					continue
				}
				if !isObject || !provided {
					ret.error = fmt.Errorf("node %s missing named pipeline output %s", n.Name, portName)
					break
				}
			} else {
				field := "structuredContent"
				if portName == "content" { field = "content" }
				var provided bool
				if ret.mcp != nil { value, provided = ret.mcp[field] }
				if !provided {
					ret.error = fmt.Errorf("node %s missing %s MCP output", n.Name, portName)
					break
				}
			}
			if err := contract.Validate(value); err != nil {
				ret.error = fmt.Errorf("node %s output port %s failed schema validation", n.Name, portName)
				break
			}
		}
	}
	return ret
}
func (r *Runtime) mcpCall(ctx context.Context, cfg *config.Config, u config.Upstream, tool string, args map[string]any, runID, node, effect string) (map[string]any, error) {
	if err := r.CheckQueuedTaskExecution(ctx); err != nil {
		return nil, err
	}
	if err := r.CheckGrantExecution(ctx); err != nil {
		return nil, err
	}
	if err := checkOutbound(u.URL); err != nil {
		return nil, err
	}
	// Acquire and verify the target token before any dispatching effect is
	// journaled. Authorization failure means no external write was attempted.
	conn, err := r.connFor(ctx, cfg, u)
	if err != nil {
		return nil, err
	}
	if r.Store != nil {
		if err = r.Store.RequireExecutionLeader(ctx); err != nil {
			return nil, err
		}
	}
	pin := u.ToolSchemaPins[tool]
	if effect == "read" {
		if err := r.checkMCPDispatch(ctx); err != nil {
			return nil, err
		}
		return r.callReadWithRetry(ctx, conn.Timeout, func(ctx context.Context) (map[string]any, error) {
			if pin != "" {
				return conn.CallPinnedTool(ctx, tool, args, pin, r.checkMCPDispatch)
			}
			return conn.CallTool(ctx, tool, args)
		})
	}

	operation := u.Name + "." + tool
	tokenArg, token, err := writeTokenArgument(cfg, operation, u, storage.EffectID(runID, node, operation))
	if err != nil {
		return nil, err
	}
	args, err = attachWriteToken(args, tokenArg, token)
	if err != nil {
		return nil, err
	}
	if r.Store == nil {
		return nil, fmt.Errorf("durable effect journal required before external write")
	}

	var attempt storage.Effect
	admitted := false
	admit := func(ctx context.Context) error {
		if err := r.checkMCPDispatch(ctx); err != nil {
			return err
		}
		// Unknown results and attempts already in flight cannot be replayed.
		var err error
		attempt, err = r.Store.BeginEffectBound(runID, node, operation, storage.TargetDigest("mcp", u.URL), args)
		if err != nil {
			return err
		}
		if err = r.checkMCPPostAdmission(ctx, attempt); err != nil {
			return err
		}
		admitted = true
		return nil
	}
	var raw map[string]any
	var e error
	if pin != "" {
		raw, e = conn.CallPinnedTool(ctx, tool, args, pin, admit)
	} else {
		if err := admit(ctx); err != nil {
			return nil, err
		}
		raw, e = conn.CallTool(ctx, tool, args)
	}
	if admitted {
		state := "confirmed"
		reason := ""
		if e != nil {
			state = "unknown"
			reason = expr.RedactDiagnostic(e.Error())
		} else if outcomeErr := validateMCPToolOutcome(raw); outcomeErr != nil {
			// A provider can commit a write and subsequently return an MCP
			// application error. Only a valid non-error tool result confirms
			// the outcome; the envelope itself stays transparent to callers.
			state = "unknown"
			reason = expr.RedactDiagnostic(outcomeErr.Error())
		}
		if err := r.Store.CompleteEffect(attempt.ID, state, reason); err != nil {
			// Never return success when the remote action completed but
			// durability of its outcome could not be established.
			return nil, fmt.Errorf("effect outcome not durable: %w", err)
		}
	}
	return raw, e
}

// Recheck authorization and the execution lease after schema discovery.
func (r *Runtime) checkMCPDispatch(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.Store != nil {
		if err := r.Store.RequireExecutionLeader(ctx); err != nil {
			return err
		}
	}
	if err := r.CheckQueuedTaskExecution(ctx); err != nil {
		return err
	}
	return r.CheckGrantExecution(ctx)
}

// checkMCPPostAdmission seals every denied write after durable admission.
// Even when the request was not sent, the conservative UNKNOWN result is
// preferable to an unresolvable dispatching journal or a speculative retry.
// It is intentionally callable by the race/regression test.
func (r *Runtime) checkMCPPostAdmission(ctx context.Context, attempt storage.Effect) error {
    if r.Store==nil {return fmt.Errorf("durable MCP effect journal unavailable")}
    err:=r.Store.RequireExecutionLeader(ctx)
    if err==nil {err=r.CheckQueuedTaskExecution(ctx)}
    if err==nil {err=r.CheckGrantExecution(ctx)}
    if err==nil {return nil}
    if completeErr:=r.Store.CompleteEffect(attempt.ID,"unknown",expr.RedactDiagnostic(err.Error()));completeErr!=nil {
        return fmt.Errorf("MCP write outcome not durable after pre-dispatch refusal: %w",completeErr)
    }
    return err
}

func (r *Runtime) runHook(ctx context.Context, h config.Hook, cfg *config.Config, value any, dry bool) (any, error) {
	if dry {
		return value, nil
	}
    if h.Transport!="builtin" {
        if err:=r.CheckQueuedTaskExecution(ctx);err!=nil{return nil,err}
        if err:=r.CheckGrantExecution(ctx);err!=nil{return nil,err}
    }
    if h.Transport!="builtin" && r.Store!=nil {
        if err:=r.Store.RequireExecutionLeader(ctx);err!=nil{return nil,err}
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
		scope,err:=requireEffectScope(ctx)
        if err!=nil{return nil,err}
        raw, e := r.mcpCall(ctx, cfg, u, h.Tool, args, scope.runID, scope.node, "unknown")
		if e != nil {
			return nil, e
		}
		if e = validateMCPToolOutcome(raw); e != nil {
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
		body, e := json.Marshal(map[string]any{"input": value})
        if e!=nil{return nil,e}
        if len(body)>maxHTTPHookRequestBytes {
            return nil,fmt.Errorf("HTTP hook request exceeds %d bytes",maxHTTPHookRequestBytes)
        }
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
		// HTTP POST is effectful even when its response is intended as a
        // transformation or authorization decision. A lost response might
        // follow an already committed external write. The SQLite barrier
        // must be durable before issuing the request.
        if r.Store==nil {return nil,fmt.Errorf("durable effect journal required before HTTP POST")}
        scope,err:=requireEffectScope(ctx)
        if err!=nil{return nil,err}
        endpointDigest:=sha256.Sum256([]byte(h.URL))
        attempt,err:=r.Store.BeginEffectBound(scope.runID,scope.node,
            fmt.Sprintf("http.POST:%x",endpointDigest),storage.TargetDigest("http",h.URL),
            map[string]any{"url":h.URL,"body":json.RawMessage(body)})
        if err!=nil{return nil,err}
        complete:=func(status string,cause error) error {
            reason:=""
            if cause!=nil {reason=expr.RedactDiagnostic(cause.Error())}
            if e:=r.Store.CompleteEffect(attempt.ID,status,reason);e!=nil {
                return fmt.Errorf("HTTP effect outcome not durable: %w",e)
            }
            return cause
        }
        if err=r.Store.RequireExecutionLeader(ctx);err!=nil {
            return nil,complete("unknown",err)
        }
        if err=r.CheckQueuedTaskExecution(ctx);err!=nil {
            return nil,complete("unknown",err)
        }
        if err=r.CheckGrantExecution(ctx);err!=nil {
            return nil,complete("unknown",err)
        }
        response,e:=hc.Do(request)
        if e!=nil {return nil,complete("unknown",e)}
        defer response.Body.Close()
        if response.StatusCode!=200 {
            return nil,complete("unknown",fmt.Errorf("hook %s: HTTP %d",h.Name,response.StatusCode))
        }
        result,e:=strictjson.ReadObject(response.Body,4<<20)
        if e!=nil {return nil,complete("unknown",fmt.Errorf("invalid HTTP hook JSON response: %w",e))}
        // This confirmation describes the HTTP response, not the policy
        // decision encoded in it. A known "deny" is not a lost effect.
        if e=complete("confirmed",nil);e!=nil {return nil,e}
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
// completeSuspendedCheckpoint advances exactly one suspended leaf. Every
// PendingSuspensionPaths exposes only node identifiers, never checkpoint
// inputs or values. A path is an array, not a slash-separated string: HCL
// quoted node names may contain path delimiters.
func PendingSuspensionPaths(cp *Checkpoint) [][]string {
	if cp == nil { return nil }
	var paths [][]string
	var walk func(*Checkpoint, []string)
	walk = func(cur *Checkpoint, prefix []string) {
		if cur == nil { return }
		if cur.FrameKind == "map" {
			walk(cur.Child, append(append([]string{}, prefix...), strconv.Itoa(cur.MapIndex)))
			return
		}
		nodes := cur.Suspensions
		if nodes == nil {
			if cur.SuspendNode == "" { return }
			nodes = map[string]*Checkpoint{cur.SuspendNode: cur.Child}
		}
		names := make([]string, 0, len(nodes))
		for name := range nodes { names = append(names, name) }
		sort.Strings(names)
		for _, name := range names {
			path := append(append([]string{}, prefix...), name)
			if child := nodes[name]; child != nil {
				walk(child, path)
			} else if cur.States[name] == "pending" {
				paths = append(paths, path)
			}
		}
	}
	walk(cp, nil)
	return paths
}

func equalSuspensionPath(a, b []string) bool {
	if len(a) != len(b) { return false }
	for i := range a { if a[i] != b[i] { return false } }
	return true
}

// validateMappedFrame verifies the frozen array, completed result prefix and
// selected iteration against the immutable map/child contracts. A checkpoint
// cannot redirect an accepted reply to an earlier (already effectful) index.
func validateMappedFrame(cfg *config.Config, pipelineName, nodeName string,
	frame *Checkpoint, runID, revision, surface, tool string) error {
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok {return fmt.Errorf("mapped checkpoint pipeline not found")}
	node, ok := pipeline.Nodes[nodeName]
	if !ok || node.Kind != "map" || node.Str("pipeline") == "" ||
		node.Attrs["allow_effects"] != true || node.Attrs["allow_suspend"] != true {
		return fmt.Errorf("mapped checkpoint not authorized")
	}
	childName := node.Str("pipeline")
	child, exists := cfg.Pipelines[childName]
	if !exists {return fmt.Errorf("mapped child pipeline not found")}
	if frame == nil || frame.FrameKind != "map" ||
		frame.RunID != mappedFrameRunID(runID, nodeName, childName) ||
		frame.Revision != revision || frame.Surface != surface || frame.Tool != tool ||
		frame.Child == nil || frame.SuspendNode != "" || len(frame.Suspensions) > 0 ||
		frame.Input != nil || frame.States != nil || frame.Values != nil || frame.Fired != nil {
		return fmt.Errorf("invalid mapped checkpoint frame identity")
	}
	limit := node.Int("max_items", 0)
	if frame.MapItems == nil || limit < 1 || limit > 64 ||
		len(frame.MapItems) == 0 || len(frame.MapItems) > limit ||
		frame.MapIndex < 0 || frame.MapIndex >= len(frame.MapItems) ||
		len(frame.MapResults) != frame.MapIndex {
		return fmt.Errorf("invalid mapped checkpoint index or completed prefix")
	}
	plan := cfg.Plans[pipelineName]
	childPlan := cfg.Plans[childName]
	if plan == nil || childPlan == nil || plan.Nodes[nodeName] == nil ||
		childPlan.Nodes[child.Entry] == nil {
		return fmt.Errorf("missing mapped continuation contracts")
	}
	inputSchema := plan.Nodes[nodeName].InputSchema
	childInput := childPlan.Nodes[child.Entry].InputSchema
	if plan.Nodes[nodeName].MapItemSchema == nil {
		return fmt.Errorf("mapped continuation lacks a compiled item contract")
	}
	// Immutable completed prefixes must satisfy the *map item* contract,
	// not just an individual child terminal. This also covers required
	// and optional fields of multi-terminal mapped children.
	for i, item := range frame.MapItems {
		arguments, ok := item.(map[string]any)
		if !ok || arguments == nil ||
			inputSchema.Validate(item) != nil || childInput.Validate(item) != nil {
			return fmt.Errorf("invalid mapped checkpoint input %d", i)
		}
	}
	for i, value := range frame.MapResults {
		if plan.Nodes[nodeName].MapItemSchema.Validate(value) != nil {
			return fmt.Errorf("invalid mapped checkpoint completed output %d", i)
		}
	}
	return nil
}

// validateSuspensionTree verifies the durable snapshot against the immutable
// pipeline contract, including every parked branch (not only the selected one).
// A completed leaf is allowed after another parallel leaf was answered, but
// cannot be answered again. Neither untrusted input nor HCL nodes can invent a
// new continuation target or bypass the allow_suspend boundary.
func validateSuspensionTree(cfg *config.Config, pipelineName string, cp *Checkpoint,
	runID, revision, surface, tool string, depth int) error {
	if depth > config.MaxPurePipelineCallDepth || cp == nil ||
		cp.FrameKind != "" ||
		cp.RunID != runID || cp.Revision != revision ||
		cp.Surface != surface || cp.Tool != tool ||
		cp.Input == nil || cp.States == nil || cp.Values == nil || cp.Fired == nil {
		return fmt.Errorf("invalid suspended checkpoint identity")
	}
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok { return fmt.Errorf("checkpoint pipeline not found") }
	if len(cp.Suspensions) > 0 && (cp.SuspendNode != "" || cp.Child != nil) {
		return fmt.Errorf("ambiguous suspended checkpoint encoding")
	}
	nodes := cp.Suspensions
	if nodes == nil {
		if cp.SuspendNode == "" { return fmt.Errorf("empty suspended checkpoint") }
		nodes = map[string]*Checkpoint{cp.SuspendNode: cp.Child}
	}
	if len(nodes) == 0 { return fmt.Errorf("empty suspended checkpoint") }
	for name, child := range nodes {
		node, exists := pipeline.Nodes[name]
		if !exists { return fmt.Errorf("unknown suspended checkpoint node") }
		switch node.Kind {
		case "suspend":
			if child != nil { return fmt.Errorf("suspend leaf cannot own child checkpoint") }
			if cp.States[name] != "pending" && cp.States[name] != "completed" {
				return fmt.Errorf("invalid suspend leaf state")
			}
			if cp.States[name] == "completed" {
				state, ok := cp.Values[name].(map[string]any)
				if !ok { return fmt.Errorf("completed suspend leaf has no value") }
				value, ok := state["value"]
				if !ok { return fmt.Errorf("completed suspend leaf has no value") }
				// Every already-accepted reply must still satisfy the pinned
				// output contract on recovery, not merely have a value key.
				plan := cfg.Plans[pipelineName]
				if plan == nil || plan.Nodes[name] == nil {
					return fmt.Errorf("completed suspend leaf contract unavailable")
				}
				if err := plan.Nodes[name].OutputSchema.Validate(value); err != nil {
					return fmt.Errorf("completed suspend leaf failed output schema: %w", err)
				}
				if port := plan.Nodes[name].OutputPorts["value"]; port != nil {
					if err := port.Validate(value); err != nil {
						return fmt.Errorf("completed suspend leaf failed value port schema: %w", err)
					}
				}
			}
		case "pipeline.call":
			if cp.States[name] != "pending" || child == nil ||
				node.Attrs["allow_suspend"] != true || node.Attrs["allow_effects"] != true {
				return fmt.Errorf("nested suspension not authorized")
			}
			childName := node.Str("pipeline")
			if err := validateSuspensionTree(cfg, childName, child,
				derivedSubrunID(runID, name, childName), revision, surface, tool, depth+1); err != nil {
				return err
			}
		case "map":
			if cp.States[name] != "pending" {
				return fmt.Errorf("mapped checkpoint node is not pending")
			}
			if err := validateMappedFrame(cfg, pipelineName, name, child,
				runID, revision, surface, tool); err != nil {
				return err
			}
			childName := node.Str("pipeline")
			if err := validateSuspensionTree(cfg, childName, child.Child,
				mappedIterationRunID(runID,name,childName,child.MapIndex),
				revision,surface,tool,depth+1); err != nil { return err }
		default:
			return fmt.Errorf("checkpoint target is not a suspension boundary")
		}
	}
	return nil
}

// completeSuspendedCheckpoint applies exactly one validated continuation. Its
// caller must check that the selected leaf path is pending before invocation.
func completeSuspendedCheckpoint(cfg *config.Config, pipelineName string,
	cp *Checkpoint, runID, revision, surface, tool string, path []string, value any, depth int) error {
	if depth > config.MaxPurePipelineCallDepth || len(path) == 0 || cp == nil {
		return fmt.Errorf("invalid suspended checkpoint path")
	}
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok { return fmt.Errorf("checkpoint pipeline not found") }
	name := path[0]
	var child *Checkpoint
	if cp.Suspensions != nil {
		var exists bool
		child, exists = cp.Suspensions[name]
		if !exists { return fmt.Errorf("unknown suspension path") }
	} else {
		if cp.SuspendNode != name { return fmt.Errorf("unknown suspension path") }
		child = cp.Child
	}
	node, ok := pipeline.Nodes[name]
	if !ok { return fmt.Errorf("checkpoint suspend node not found") }
	switch node.Kind {
	case "pipeline.call":
		if child == nil || node.Attrs["allow_suspend"] != true ||
			node.Attrs["allow_effects"] != true || cp.States[name] != "pending" {
			return fmt.Errorf("nested suspension not authorized")
		}
		childName := node.Str("pipeline")
		return completeSuspendedCheckpoint(cfg, childName, child,
			derivedSubrunID(runID, node.Name, childName),
			revision, surface, tool, path[1:], value, depth+1)
	case "map":
		if cp.States[name] != "pending" || len(path) < 3 {
			return fmt.Errorf("mapped suspension path invalid")
		}
		if err := validateMappedFrame(cfg,pipelineName,name,child,
			runID,revision,surface,tool);err!=nil {return err}
		if path[1] != strconv.Itoa(child.MapIndex) {
			return fmt.Errorf("unknown mapped suspension iteration")
		}
		childName:=node.Str("pipeline")
		return completeSuspendedCheckpoint(cfg,childName,child.Child,
			mappedIterationRunID(runID,name,childName,child.MapIndex),
			revision,surface,tool,path[2:],value,depth+1)
	case "suspend":
		if len(path) != 1 || child != nil || cp.States[name] != "pending" {
			return fmt.Errorf("suspend leaf already answered or path invalid")
		}
		plan := cfg.Plans[pipelineName]
		if plan == nil || plan.Nodes[name] == nil {
			return fmt.Errorf("suspend output contract unavailable")
		}
		if err := plan.Nodes[name].OutputSchema.Validate(value); err != nil {
			return fmt.Errorf("suspend continuation failed output schema: %w", err)
		}
		if port := plan.Nodes[name].OutputPorts["value"]; port != nil {
			if err := port.Validate(value); err != nil {
				return fmt.Errorf("suspend continuation failed value port schema: %w", err)
			}
		}
		cp.States[name] = "completed"
		cp.Values[name] = map[string]any{"value": value, "status": "success"}
		for _, edge := range pipeline.Edges {
			if edge.Kind != "data" && edge.From == name &&
				(edge.When == "" || edge.When == "always" || edge.When == fmt.Sprint(value)) {
				cp.Fired[edge.From+">"+edge.To] = true
			}
		}
		return nil
	default:
		return fmt.Errorf("checkpoint target is not a suspension boundary")
	}
}

func (r *Runtime) Resume(ctx context.Context, cfg *config.Config, revision string, runID string, value any) (Outcome, error) {
	return r.ResumeAt(ctx, cfg, revision, runID, nil, value)
}

// ResumeAt selects one leaf suspension by its nested node-name path. A nil
// path preserves the old API only when exactly one leaf is pending.
func (r *Runtime) ResumeAt(ctx context.Context, cfg *config.Config, revision string, runID string, path []string, value any) (Outcome, error) {
	out := Outcome{RunID: runID, Revision: revision, Status: "failure"}
	if r.Store == nil { return out, fmt.Errorf("durable checkpoint store required") }
	ctx = withEffectScope(ctx, runID)
	// Hold the per-run guard across the whole resume, including downstream
	// effects and checkpoint deletion. A second answer must read the updated
	// snapshot; otherwise concurrent independent leaf answers can be lost.
	unlock := r.Store.LockSuspendedRun(runID)
	defer unlock()
	// Re-check authorization and leader fencing after waiting for the lock.
	if err := r.CheckGrantExecution(ctx); err != nil { return out, err }
	if err := r.Store.RequireExecutionLeader(ctx); err != nil { return out, err }
	var cp Checkpoint
	if err := r.Store.GetSuspended(runID, &cp); err != nil { return out, err }
	if cp.Revision != revision {
		return out, fmt.Errorf("resume requires revision %s, received %s", cp.Revision, revision)
	}
	surface, exists := cfg.Surfaces[cp.Surface]
	if !exists { return out, fmt.Errorf("missing original surface") }
	pipelineName := ""
	var selected config.Tool
	for _, tool := range surface.Tools {
		if tool.Name == cp.Tool { pipelineName = tool.Pipeline; selected = tool; break }
	}
	if pipelineName == "" { return out, fmt.Errorf("missing original tool") }
	if err := enforceDataTrust(cfg, cp.Surface, selected); err != nil {
		out.Status = "denied"
		out.Error = "untrusted destination"
		return out, err
	}
	pipeline, ok := cfg.Pipelines[pipelineName]
	if !ok { return out, fmt.Errorf("missing original pipeline") }
	if err := validateSuspensionTree(cfg, pipelineName, &cp, runID,
		revision, cp.Surface, cp.Tool, 1); err != nil { return out, err }
	pending := PendingSuspensionPaths(&cp)
	if len(pending) == 0 {
		// A crash or a downstream graph failure can leave a checkpoint
		// whose final leaf answer was already durably accepted. Re-drive
		// that immutable snapshot, never accept a replacement answer.
		if len(path) != 0 || value != nil {
			return out, fmt.Errorf("accepted continuation is already durable; recovery forbids new input")
		}
	} else {
		if len(path) == 0 {
			if len(pending) != 1 { return out, fmt.Errorf("resume requires an explicit suspension path") }
			path = pending[0]
		}
		found := false
		for _, candidate := range pending {
			if equalSuspensionPath(path, candidate) { found = true; break }
		}
		if !found { return out, fmt.Errorf("unknown or already answered suspension path") }
		if err := completeSuspendedCheckpoint(cfg, pipelineName, &cp, runID,
			revision, cp.Surface, cp.Tool, path, value, 1); err != nil {
			return out, err
		}
		// Commit EVERY accepted response, including the final leaf, before
		// executing any downstream node. A crash after this barrier must
		// not let a caller replace the answer or replay a remote effect.
		if err := r.Store.PutSuspended(runID, &cp); err != nil {
			return out, fmt.Errorf("persist accepted continuation: %w", err)
		}
		if remaining := PendingSuspensionPaths(&cp); len(remaining) > 0 {
			out.Status = "suspended"
			out.Checkpoint = &cp
			out.SuspensionPaths = remaining
			out.Value = map[string]any{"run_id": runID, "paths": remaining}
			return out, fmt.Errorf("waiting for %d suspension(s)", len(remaining))
		}
	}
	runctx, cancel := context.WithTimeout(ctx, time.Duration(pipeline.TimeoutMS)*time.Millisecond)
	defer cancel()
	out, err := r.execute(runctx, revision, cfg, pipeline, runID,
		cp.Surface, cp.Tool, cp.Input, false, &cp)
	// A completed resumed root can still have parallel terminal results
	// persisted before its suspension. Validate the complete set before
	// publishing a response, and discard the consumed checkpoint even if
	// the result is ambiguous: re-resume must never replay post-resume effects.
	completed := err == nil && out.Status != "suspended"
	if completed {
		err = requireUniqueSurfaceOutput(&out, pipeline.Name)
		if err == nil {
			err = r.finalizePublishedOutput(ctx, cfg, cp.Surface, cp.Tool,
				cfg.ToolContracts[cp.Surface][cp.Tool].Output, &out, false)
		}
		if err != nil {
			out.Status = "failure"
			out.Error = expr.RedactDiagnostic(err.Error())
			if surface.OAuth != "" { out.Error = "operation failed" }
			_, _ = r.intercept(ctx, cfg, "on_error", cp.Surface, cp.Tool,
				map[string]any{"status": "failure", "message": out.Error, "run_id": runID}, false)
			r.Store.WriteTrace(storage.Trace{RunID:runID, Revision:revision,
				Surface:cp.Surface, Tool:cp.Tool, Status:"failure", Message:out.Error})
		}
	}
	if out.Status == "suspended" {
		if out.Checkpoint == nil {
			out.Status = "failure"
			scrubPublishedResult(&out)
			return out, fmt.Errorf("nested continuation checkpoint unavailable")
		}
		// execute has already atomically replaced the root checkpoint.
	} else if completed {
		if e := r.Store.DeleteSuspended(runID); e != nil {
			out.Status = "failure"
			scrubPublishedResult(&out)
			return out, fmt.Errorf("clear completed checkpoint: %w", e)
		}
	}
	if err != nil && out.Status != "suspended" {
		scrubPublishedResult(&out)
		out.Status = "failure"
		if out.Error == "" {
			out.Error = expr.RedactDiagnostic(err.Error())
			if surface.OAuth != "" { out.Error = "operation failed" }
		}
		if !completed {
			// A failing resumed *graph* keeps its checkpoint so the
			// operator can correct the continuation. Never expose values
			// from nodes that completed before the failure.
			_, _ = r.intercept(ctx, cfg, "on_error", cp.Surface, cp.Tool,
				map[string]any{"status": "failure", "message": out.Error, "run_id": runID}, false)
			r.Store.WriteTrace(storage.Trace{RunID:runID, Revision:revision,
				Surface:cp.Surface, Tool:cp.Tool, Status:"failure", Message:out.Error})
		}
	}
	return out, err
}

// ReadResource forwards an MCP resources/read request, preserving the upstream content shape.
func (r *Runtime) ReadResource(ctx context.Context,cfg *config.Config,u config.Upstream,uri string)(map[string]any,error) {
    if e:=r.CheckQueuedTaskExecution(ctx);e!=nil{return nil,e}
    if e:=r.CheckGrantExecution(ctx);e!=nil{return nil,e}
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
