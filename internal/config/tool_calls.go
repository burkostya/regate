package config

import (
	"fmt"
	"strings"
)

func validateDeriveSettings(s Surface) error {
	if s.Upstream != "" && s.DeriveRules {
		return fmt.Errorf("surface %s: derive_rules requires a curated surface", s.Name)
	}
	if !s.DeriveRules && len(s.DeriveSessionHeaders)+len(s.DeriveSessionMetaKeys) != 0 {
		return fmt.Errorf("surface %s: session sources require derive_rules", s.Name)
	}
	if len(s.DeriveSessionHeaders)+len(s.DeriveSessionMetaKeys) > 16 {
		return fmt.Errorf("surface %s: at most 16 session sources are allowed", s.Name)
	}
	headers := map[string]bool{}
	for _, name := range s.DeriveSessionHeaders {
		key := strings.ToLower(name)
		if name == "" || strings.IndexFunc(name, func(r rune) bool {
			return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!#$%&'*+-.^_`|~", r)
		}) >= 0 || len(name) > 128 || headers[key] ||
			key == "authorization" || key == "proxy-authorization" || key == "cookie" {
			return fmt.Errorf("surface %s: invalid or duplicate session header", s.Name)
		}
		headers[key] = true
	}
	keys := map[string]bool{}
	for _, key := range s.DeriveSessionMetaKeys {
		if len(key) > 128 || strings.TrimSpace(key) != key || key == "" || keys[key] {
			return fmt.Errorf("surface %s: invalid or duplicate session metadata key", s.Name)
		}
		keys[key] = true
	}
	return nil
}

func (s Surface) FindTool(name string) (Tool, bool) {
	for _, tool := range s.Tools {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// VisitPipelineNodes walks nested pipelines in the same published tool.
func (c *Config) VisitPipelineNodes(root string, visit func(Node) error) error {
	seen := map[string]bool{}
	var walk func(string) error
	walk = func(name string) error {
		if name == "" || seen[name] {
			return nil
		}
		seen[name] = true
		pipeline, ok := c.Pipelines[name]
		if !ok {
			return fmt.Errorf("missing pipeline %s", name)
		}
		for _, node := range pipeline.Nodes {
			if err := visit(node); err != nil {
				return err
			}
			if node.Kind == "pipeline.call" || node.Kind == "map" {
				if err := walk(node.Str("pipeline")); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root)
}

// ToolTraits reports potentially effectful work and suspension in the tool.
// A writable or unknown destination is never considered read-only.
func (c *Config) ToolTraits(surface string, tool Tool) (effectful, suspendable bool) {
	for _, i := range c.Interceptors {
		if kind, _ := i.ContentTarget(); kind != "" {
			continue
		}
		if (i.Surface == "" || i.Surface == surface) && (i.Tool == "" || i.Tool == tool.Name) &&
			c.Hooks[i.Hook].Transport != "builtin" {
			effectful = true
		}
	}
	if tool.Source != "" {
		return effectful || !tool.ReadOnly, false
	}
	err := c.VisitPipelineNodes(tool.Pipeline, func(n Node) error {
		switch n.Kind {
		case "suspend":
			suspendable = true
		case "mcp.call":
			effectful = effectful || n.Str("effect") != "read"
		case "http.call":
			effectful = true
		case "hook.call":
			effectful = effectful || c.Hooks[n.Str("hook")].Transport != "builtin"
		case "policy.check":
			hook := c.Policies[n.Str("policy")].DecisionHook
			effectful = effectful || hook != "" && c.Hooks[hook].Transport != "builtin"
		case "tool.call":
			effectful = effectful || n.Bool("allow_effects", false)
		}
		return nil
	})
	return effectful || err != nil, suspendable || err != nil
}

func (c *Config) validateToolCalls() error {
	hasCalls := false
	for _, p := range c.Pipelines {
		for _, n := range p.Nodes {
			if n.Kind != "tool.call" {
				continue
			}
			hasCalls = true
			surface := c.Surfaces[n.Str("surface")]
			child, ok := surface.FindTool(n.Str("tool"))
			if !ok || surface.Upstream != "" {
				return fmt.Errorf("pipeline %s node %s: tool.call target is not a curated tool", p.Name, n.Name)
			}
			effectful, suspension := c.ToolTraits(surface.Name, child)
			if suspension {
				return fmt.Errorf("pipeline %s node %s: tool.call target can suspend", p.Name, n.Name)
			}
			if effectful && !n.Bool("allow_effects", false) {
				return fmt.Errorf("pipeline %s node %s: tool.call requires allow_effects=true", p.Name, n.Name)
			}
		}
	}
	if !hasCalls {
		return nil
	}
	for _, surface := range c.Surfaces {
		calls := make(map[string][]string)
		for _, tool := range surface.Tools {
			err := c.VisitPipelineNodes(tool.Pipeline, func(n Node) error {
				if n.Kind != "tool.call" {
					return nil
				}
				if n.Str("surface") != surface.Name {
					return fmt.Errorf("tool %s: tool.call must target its own surface", tool.Name)
				}
				child, _ := surface.FindTool(n.Str("tool"))
				for _, scope := range child.RequiredScopes {
					if !containsScope(tool.RequiredScopes, scope) && !containsScope(surface.RequiredScopes, scope) {
						return fmt.Errorf("tool %s: tool.call requires scope %s of %s", tool.Name, scope, child.Name)
					}
				}
				calls[tool.Name] = append(calls[tool.Name], child.Name)
				return nil
			})
			if err != nil {
				return err
			}
		}
		height := map[string]int{}
		active := map[string]bool{}
		var depth func(string, int) (int, error)
		depth = func(name string, level int) (int, error) {
			if active[name] {
				return 0, fmt.Errorf("tool %s: cyclic tool.call dependency", name)
			}
			if level > MaxPurePipelineCallDepth {
				return 0, fmt.Errorf("tool %s: tool.call depth exceeds %d", name, MaxPurePipelineCallDepth)
			}
			if h := height[name]; h != 0 {
				return h, nil
			}
			active[name] = true
			defer delete(active, name)
			h := 1
			for _, child := range calls[name] {
				sub, err := depth(child, level+1)
				if err != nil {
					return 0, err
				}
				h = max(h, sub+1)
			}
			if level+h-1 > MaxPurePipelineCallDepth {
				return 0, fmt.Errorf("tool %s: tool.call depth exceeds %d", name, MaxPurePipelineCallDepth)
			}
			height[name] = h
			return h, nil
		}
		for _, tool := range surface.Tools {
			if _, err := depth(tool.Name, 1); err != nil {
				return err
			}
		}
	}
	return nil
}
