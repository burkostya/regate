package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type Config struct {
	Listen        string                             `json:"listen"`
    OAuth         map[string]OAuth                    `json:"oauth,omitempty"`
	Upstreams     map[string]Upstream                `json:"upstreams"`
	Hooks         map[string]Hook                    `json:"hooks"`
	Policies      map[string]Policy                  `json:"policies"`
	Pipelines     map[string]Pipeline                `json:"pipelines"`
	Surfaces      map[string]Surface                 `json:"surfaces"`
	Interceptors  []Interceptor                      `json:"interceptors"`
	Source        string                             `json:"-"`
	Plans         map[string]*PipelineIR             `json:"-"`
	ToolContracts map[string]map[string]ToolContract `json:"-"`
}
// OAuth defines an explicit per-surface trusted issuer and resource audience.
 // JWKS authenticates self-contained signed JWTs; introspection supports opaque
 // access tokens. Both are optional individually, but at least one is required.
type OAuth struct {
    Name string `json:"name"`
    Issuer string `json:"issuer"`
    Audience string `json:"audience"`
    JWKSURL string `json:"jwks_url,omitempty"`
    IntrospectionURL string `json:"introspection_url,omitempty"`
    ClientIDEnv string `json:"client_id_env,omitempty"`
    ClientSecretEnv string `json:"client_secret_env,omitempty"`
}
type Upstream struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	AuthEnv   string `json:"auth_env,omitempty"`
	TimeoutMS int    `json:"timeout_ms"`
}
type Hook struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	URL       string `json:"url,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Tool      string `json:"tool,omitempty"`
	AuthEnv   string `json:"auth_env,omitempty"`
	Contract  string `json:"contract"`
	OnError   string `json:"on_error"`
	TimeoutMS int    `json:"timeout_ms"`
}
type Policy struct {
	Name         string `json:"name"`
	DecisionHook string `json:"decision_hook,omitempty"`
	Scope        string `json:"scope,omitempty"`
}
type Surface struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
    OAuth     string     `json:"oauth,omitempty"`
    RequiredScopes []string `json:"required_scopes,omitempty"`
	Tools     []Tool     `json:"tools"`
	Resources []Resource `json:"resources,omitempty"`
	Prompts   []Prompt   `json:"prompts,omitempty"`
}
type Tool struct {
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Pipeline     string         `json:"pipeline,omitempty"`
	Source       string         `json:"source,omitempty"`
	InputSchema  map[string]any `json:"input_schema,omitempty"`
	OutputSchema map[string]any `json:"output_schema,omitempty"`
	ReadOnly     bool           `json:"read_only"`
    RequiredScopes []string `json:"required_scopes,omitempty"`
    DataClassification string `json:"data_classification,omitempty"`
    AllowUnmasked bool `json:"allow_unmasked,omitempty"`
}
type Resource struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	Source   string `json:"source,omitempty"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
    RequiredScopes []string `json:"required_scopes,omitempty"`
}
type Prompt struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Text        string `json:"text"`
    RequiredScopes []string `json:"required_scopes,omitempty"`
}
type Pipeline struct {
	Name        string          `json:"name"`
	Entry       string          `json:"entry"`
	Nodes       map[string]Node `json:"nodes"`
	Edges       []Edge          `json:"edges"`
	TimeoutMS   int             `json:"timeout_ms"`
	Concurrency int             `json:"concurrency"`
}
type Node struct {
	Name  string         `json:"name"`
	Kind  string         `json:"kind"`
	Attrs map[string]any `json:"attrs"`
	Span  SourceRange    `json:"-"`
}
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	When string `json:"when,omitempty"`
}
type Interceptor struct {
	Name    string `json:"name"`
	Stage   string `json:"stage"`
	Hook    string `json:"hook"`
	Surface string `json:"surface,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Order   int    `json:"order"`
}

func Decode(src string) (*Config, error) {
	blocks, err := Parse(src)
	if err != nil {
		return nil, err
	}
	if err = ValidateBlockSchema(blocks); err != nil {
		return nil, err
	}
	c := &Config{Listen: "127.0.0.1:8080", OAuth: map[string]OAuth{}, Upstreams: map[string]Upstream{}, Hooks: map[string]Hook{}, Policies: map[string]Policy{}, Pipelines: map[string]Pipeline{}, Surfaces: map[string]Surface{}, Source: src}
	seen := map[string]bool{}
	for _, b := range blocks {
		name := ""
		if b.Type != "regate" {
			name, err = b.Label()
			if err != nil {
				return nil, err
			}
		}
		key := b.Type + ":" + name
		if seen[key] {
			return nil, fmt.Errorf("duplicate block %s", key)
		}
		seen[key] = true
		switch b.Type {
		case "regate":
			if s := b.String("listen"); s != "" {
				c.Listen = s
			}
		case "oauth":
            c.OAuth[name] = OAuth{Name:name,Issuer:b.String("issuer"), Audience:b.String("audience"),JWKSURL:b.String("jwks_url"),
                IntrospectionURL:b.String("introspection_url"),ClientIDEnv:b.String("client_id_env"),ClientSecretEnv:b.String("client_secret_env")}
		case "upstream":
			u := Upstream{Name: name, URL: b.String("url"), AuthEnv: b.String("auth_env"), TimeoutMS: b.Int("timeout_ms", 15000)}
			c.Upstreams[name] = u
		case "hook":
			h := Hook{Name: name, Transport: b.String("transport"), URL: b.String("url"), Upstream: b.String("upstream"), Tool: b.String("tool"), AuthEnv: b.String("auth_env"), Contract: b.String("contract"), OnError: b.String("on_error"), TimeoutMS: b.Int("timeout_ms", 10000)}
			if h.OnError == "" {
				h.OnError = "deny"
			}
			c.Hooks[name] = h
		case "policy":
			c.Policies[name] = Policy{Name: name, DecisionHook: b.String("decision_hook"), Scope: b.String("scope")}
		case "pipeline":
			p := Pipeline{Name: name, Entry: b.String("entry"), Nodes: map[string]Node{}, TimeoutMS: b.Int("timeout_ms", 60000), Concurrency: b.Int("concurrency", 8)}
			for _, ch := range b.Children {
				switch ch.Type {
				case "node":
					n, e := ch.Label()
					if e != nil {
						return nil, e
					}
					if _, ok := p.Nodes[n]; ok {
						return nil, fmt.Errorf("duplicate node %s", n)
					}
					p.Nodes[n] = Node{Name: n, Kind: ch.String("kind"), Attrs: ch.Attrs, Span: SourceRange{Start: ch.Start, End: ch.End, Line: ch.Line, Column: 1}}
				case "edge":
					p.Edges = append(p.Edges, Edge{From: ch.String("from"), To: ch.String("to"), When: ch.String("when")})
				default:
					return nil, fmt.Errorf("invalid nested block %s in pipeline", ch.Type)
				}
			}
			c.Pipelines[name] = p
		case "surface":
			s := Surface{Name:name,Path:b.String("path"),OAuth:b.String("oauth"),RequiredScopes:stringList(b.Attrs["required_scopes"])}
			for _, ch := range b.Children {
				n, e := ch.Label()
				if e != nil {
					return nil, e
				}
				switch ch.Type {
				case "tool":
					schema, _ := ch.Attrs["input_schema"].(map[string]any)
					out, _ := ch.Attrs["output_schema"].(map[string]any)
					s.Tools = append(s.Tools, Tool{Name: n, Description: ch.String("description"), Pipeline: ch.String("pipeline"), Source: ch.String("source"), InputSchema: schema, OutputSchema: out, ReadOnly: ch.Bool("read_only", false),RequiredScopes:stringList(ch.Attrs["required_scopes"]),DataClassification:ch.String("data_classification"),AllowUnmasked:ch.Bool("allow_unmasked",false)})
				case "resource":
					s.Resources = append(s.Resources, Resource{Name: n, URI: ch.String("uri"), Source: ch.String("source"), Text: ch.String("text"), MimeType: ch.String("mime_type"),RequiredScopes:stringList(ch.Attrs["required_scopes"])})
				case "prompt":
					s.Prompts = append(s.Prompts, Prompt{Name: n, Description: ch.String("description"), Text: ch.String("text"),RequiredScopes:stringList(ch.Attrs["required_scopes"])})
				default:
					return nil, fmt.Errorf("invalid %s in surface", ch.Type)
				}
			}
			c.Surfaces[name] = s
		case "interceptor":
			c.Interceptors = append(c.Interceptors, Interceptor{Name: name, Stage: b.String("stage"), Hook: b.String("hook"), Surface: b.String("surface"), Tool: b.String("tool"), Order: b.Int("order", 100)})
		default:
			return nil, fmt.Errorf("unknown HCL block %s", b.Type)
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if err := c.Compile(); err != nil {
		return nil, err
	}
	return c, nil
}
func stringList(v any) []string {
    raw,_:=v.([]any)
    out:=make([]string,0,len(raw))
    for _,item:=range raw {if s,ok:=item.(string);ok {out=append(out,s)}}
    return out
}
func validateScopeList(v []string) error {
    seen:=map[string]bool{}
    for _,scope:=range v {
        if strings.TrimSpace(scope)!=scope || strings.ContainsAny(scope," \t\n\r") || len(scope)==0 || seen[scope] {
            return fmt.Errorf("scope must be unique nonempty token without whitespace")
        }
        seen[scope]=true
    }
    return nil
}
func (c *Config) Validate() error {
	if len(c.Pipelines) == 0 && len(c.Surfaces) == 0 {
		return fmt.Errorf("no surfaces or pipelines")
	}
	paths := map[string]bool{}
    for name,provider := range c.OAuth {
        if provider.Issuer=="" || provider.Audience=="" {return fmt.Errorf("oauth %s: issuer and audience required",name)}
        if err:=validURL(provider.Issuer);err!=nil {return fmt.Errorf("oauth %s issuer: %w",name,err)}
        if provider.JWKSURL=="" && provider.IntrospectionURL=="" {return fmt.Errorf("oauth %s: jwks_url or introspection_url required",name)}
        if provider.JWKSURL!="" {
            if err:=validURL(provider.JWKSURL);err!=nil {return fmt.Errorf("oauth %s JWKS: %w",name,err)}
        }
        if provider.IntrospectionURL!="" {
            if err:=validURL(provider.IntrospectionURL);err!=nil {return fmt.Errorf("oauth %s introspection: %w",name,err)}
            if provider.ClientIDEnv=="" || provider.ClientSecretEnv=="" {return fmt.Errorf("oauth %s introspection requires client_id_env and client_secret_env",name)}
        }
        for _,key:=range []string{provider.ClientIDEnv,provider.ClientSecretEnv} {
            if key!="" && !safeEnvName(key) {return fmt.Errorf("oauth %s: invalid secret environment variable",name)}
        }
    }
	for name, u := range c.Upstreams {
		if err := validURL(u.URL); err != nil {
			return fmt.Errorf("upstream %s: %w", name, err)
		}
	}
	for name, h := range c.Hooks {
		if h.Transport != "http" && h.Transport != "mcp" && h.Transport != "builtin" {
			return fmt.Errorf("hook %s: transport must be http/mcp/builtin", name)
		}
		if h.Transport == "http" {
			if err := validURL(h.URL); err != nil {
				return fmt.Errorf("hook %s: %w", name, err)
			}
		}
		if h.Transport == "mcp" {
			if _, ok := c.Upstreams[h.Upstream]; !ok {
				return fmt.Errorf("hook %s: missing upstream %s", name, h.Upstream)
			}
			if h.Tool == "" {
				return fmt.Errorf("hook %s: missing tool", name)
			}
		}
		if h.Contract != "transform" && h.Contract != "decision" && h.Contract != "observe" && h.Contract != "action" {
			return fmt.Errorf("hook %s: unknown contract", name)
		}
	}
	for name, p := range c.Policies {
		if p.DecisionHook != "" {
			h, ok := c.Hooks[p.DecisionHook]
			if !ok || h.Contract != "decision" {
				return fmt.Errorf("policy %s: decision hook required", name)
			}
		}
	}
	for name, p := range c.Pipelines {
		if len(p.Nodes) == 0 {
			return fmt.Errorf("pipeline %s has no nodes", name)
		}
		if _, ok := p.Nodes[p.Entry]; !ok {
			return fmt.Errorf("pipeline %s: entry %s not found", name, p.Entry)
		}
		for n, node := range p.Nodes {
			switch node.Kind {
			case "input", "output", "transform", "filter", "validate", "condition", "parallel", "join", "mcp.call", "hook.call", "http.call", "policy.check", "suspend", "mask.pii":
			default:
				return fmt.Errorf("pipeline %s node %s: unsupported kind %s", name, n, node.Kind)
			}
			if node.Kind == "mcp.call" {
				if _, ok := c.Upstreams[node.Str("upstream")]; !ok {
					return fmt.Errorf("pipeline %s node %s: missing upstream", name, n)
				}
			}
			if node.Kind == "hook.call" {
				if _, ok := c.Hooks[node.Str("hook")]; !ok {
					return fmt.Errorf("pipeline %s node %s: missing hook", name, n)
				}
			}
			if node.Kind == "policy.check" {
				if _, ok := c.Policies[node.Str("policy")]; !ok {
					return fmt.Errorf("pipeline %s node %s: missing policy", name, n)
				}
			}
			if node.Kind == "http.call" {
				if e := validURL(node.Str("url")); e != nil {
					return fmt.Errorf("pipeline %s node %s: %w", name, n, e)
				}
			}
		}
		adj := map[string][]string{}
		ind := map[string]int{}
		for n := range p.Nodes {
			ind[n] = 0
		}
		for _, e := range p.Edges {
			if _, ok := p.Nodes[e.From]; !ok {
				return fmt.Errorf("pipeline %s: missing from node %s", name, e.From)
			}
			if _, ok := p.Nodes[e.To]; !ok {
				return fmt.Errorf("pipeline %s: missing to node %s", name, e.To)
			}
			adj[e.From] = append(adj[e.From], e.To)
			ind[e.To]++
		}
		visiting := map[string]bool{}
		visited := map[string]bool{}
		var walk func(string) error
		walk = func(n string) error {
			if visiting[n] {
				return fmt.Errorf("pipeline %s: cycle at %s", name, n)
			}
			if visited[n] {
				return nil
			}
			visiting[n] = true
			for _, to := range adj[n] {
				if e := walk(to); e != nil {
					return e
				}
			}
			visiting[n] = false
			visited[n] = true
			return nil
		}
		for n := range p.Nodes {
			if e := walk(n); e != nil {
				return e
			}
		}
		if len(visited) == 0 {
			return fmt.Errorf("empty pipeline")
		}
		reachable := map[string]bool{}
		var reach func(string)
		reach = func(n string) {
			if reachable[n] {
				return
			}
			reachable[n] = true
			for _, to := range adj[n] {
				reach(to)
			}
		}
		reach(p.Entry)
		for n := range p.Nodes {
			if !reachable[n] {
				return fmt.Errorf("pipeline %s: unreachable node %s", name, n)
			}
		}
		if p.TimeoutMS < 1 || p.Concurrency < 1 {
			return fmt.Errorf("pipeline %s invalid limits", name)
		}
	}
	for name, s := range c.Surfaces {
        if s.OAuth!="" {
            if _,ok:=c.OAuth[s.OAuth];!ok {return fmt.Errorf("surface %s: undefined oauth provider",name)}
        } else if len(s.RequiredScopes)>0 {
            return fmt.Errorf("surface %s: required_scopes require oauth provider",name)
        }
        if err:=validateScopeList(s.RequiredScopes);err!=nil {return fmt.Errorf("surface %s: %w",name,err)}
		if s.Path == "" {
			return fmt.Errorf("surface %s: empty path", name)
		}
		if paths[s.Path] {
			return fmt.Errorf("duplicate path %s", s.Path)
		}
		paths[s.Path] = true
		if s.Path == "/mcp/admin" || strings.HasPrefix(s.Path, "/api/") {
			return fmt.Errorf("surface %s overlaps reserved path", name)
		}
		names := map[string]bool{}
		for _, t := range s.Tools {
			if names[t.Name] {
				return fmt.Errorf("duplicate tool %s in %s", t.Name, name)
			}
			names[t.Name] = true
            if err:=validateScopeList(t.RequiredScopes);err!=nil {return fmt.Errorf("tool %s: %w",t.Name,err)}
            if len(t.RequiredScopes)>0 && s.OAuth=="" {return fmt.Errorf("tool %s: scopes require surface OAuth",t.Name)}
            switch t.DataClassification {
            case "", "public", "restricted":
            default: return fmt.Errorf("tool %s: data_classification must be public or restricted",t.Name)
            }
            if t.DataClassification=="restricted" && (s.OAuth=="" || len(t.RequiredScopes)==0) {
                return fmt.Errorf("tool %s: restricted data needs OAuth and required_scopes",t.Name)
            }
            if t.AllowUnmasked && (s.OAuth=="" || t.DataClassification!="restricted" || !containsScope(t.RequiredScopes,"pii:read")) {
                return fmt.Errorf("tool %s: allow_unmasked requires OAuth restricted data and pii:read scope",t.Name)
            }
			if t.Pipeline == "" && t.Source == "" {
				return fmt.Errorf("tool %s: needs pipeline or source", t.Name)
			}
			if t.Pipeline != "" && t.Source != "" {
				return fmt.Errorf("tool %s: pipeline and source are exclusive", t.Name)
			}
			if t.Pipeline != "" {
				if _, ok := c.Pipelines[t.Pipeline]; !ok {
					return fmt.Errorf("tool %s: missing pipeline %s", t.Name, t.Pipeline)
				}
			}
			if t.Source != "" {
				u, _, ok := strings.Cut(t.Source, ".")
				if !ok {
					return fmt.Errorf("tool %s: source must be upstream.tool", t.Name)
				}
				if _, ok := c.Upstreams[u]; !ok {
					return fmt.Errorf("tool %s: upstream %s missing", t.Name, u)
				}
			}
		}
        for _,res:=range s.Resources {
            if err:=validateScopeList(res.RequiredScopes);err!=nil {return fmt.Errorf("resource %s: %w",res.Name,err)}
            if len(res.RequiredScopes)>0 && s.OAuth=="" {return fmt.Errorf("resource %s: scopes require surface OAuth",res.Name)}
        }
        for _,prompt:=range s.Prompts {
            if err:=validateScopeList(prompt.RequiredScopes);err!=nil {return fmt.Errorf("prompt %s: %w",prompt.Name,err)}
            if len(prompt.RequiredScopes)>0 && s.OAuth=="" {return fmt.Errorf("prompt %s: scopes require surface OAuth",prompt.Name)}
        }
	}
	for _, i := range c.Interceptors {
		if _, ok := c.Hooks[i.Hook]; !ok {
			return fmt.Errorf("interceptor %s: missing hook %s", i.Name, i.Hook)
		}
		if i.Stage != "on_request" && i.Stage != "before_call" && i.Stage != "after_call" && i.Stage != "before_response" && i.Stage != "on_error" {
			return fmt.Errorf("interceptor %s: invalid stage", i.Name)
		}
	}
	sort.Slice(c.Interceptors, func(i, j int) bool {
		if c.Interceptors[i].Order != c.Interceptors[j].Order {
			return c.Interceptors[i].Order < c.Interceptors[j].Order
		}
		return c.Interceptors[i].Name < c.Interceptors[j].Name
	})
	return nil
}
func containsScope(scopes []string, wanted string) bool {
    for _,scope:=range scopes {if scope==wanted{return true}}
    return false
}
func safeEnvName(s string) bool {
    if s=="" {return false}
    for _,r:=range s {
        if !(r>='A'&&r<='Z'||r>='a'&&r<='z'||r>='0'&&r<='9'||r=='_') {return false}
    }
    return true
}
func validURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil {
		return e
	}
	if u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("must be absolute HTTP(S) URL without userinfo")
	}
	return nil
}
func (n Node) Str(key string) string { s, _ := n.Attrs[key].(string); return s }
func (n Node) Bool(key string, def bool) bool {
	b, ok := n.Attrs[key].(bool)
	if !ok {
		return def
	}
	return b
}
func (n Node) Int(key string, def int) int {
	v, ok := n.Attrs[key].(int64)
	if !ok {
		return def
	}
	return int(v)
}
