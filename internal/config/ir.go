package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"cel.dev/cel-go/cel"
	"github.com/burkostya/regate/internal/expr"
)

// PortType describes the contract guaranteed by an IR port. Dynamic values
// must still satisfy the public JSON Schema at the MCP boundary; compilation
// cannot prove a remote tool's data shape from an untrusted declaration.
type PortType string

const (
	PortAny    PortType = "dynamic"
	PortObject PortType = "object"
	PortBool   PortType = "bool"
)

type NodeIR struct {
	Name         string
	Kind         string
	Span         SourceRange
	Output       PortType
	Expressions  map[string]*expr.Value
	Dependencies []string
}
type PipelineIR struct {
	Name  string
	Entry string
	Order []string // Deterministic topological order.
	Nodes map[string]*NodeIR
}

var nodeRef = regexp.MustCompile(`\bnodes\.([a-zA-Z_][a-zA-Z0-9_]*)`)

func (c *Config) Compile() error {
	plans := map[string]*PipelineIR{}
	for name, p := range c.Pipelines {
		plan := &PipelineIR{Name: name, Entry: p.Entry, Nodes: map[string]*NodeIR{}}
		forward := map[string][]string{}
		incoming := map[string]int{}
		for id := range p.Nodes {
			incoming[id] = 0
		}
		for _, e := range p.Edges {
			forward[e.From] = append(forward[e.From], e.To)
			incoming[e.To]++
		}
		ready := []string{}
		for id, count := range incoming {
			if count == 0 {
				ready = append(ready, id)
			}
		}
		for len(ready) > 0 {
			sort.Strings(ready)
			next := ready[0]
			ready = ready[1:]
			plan.Order = append(plan.Order, next)
			for _, to := range forward[next] {
				incoming[to]--
				if incoming[to] == 0 {
					ready = append(ready, to)
				}
			}
		}
		if len(plan.Order) != len(p.Nodes) {
			return fmt.Errorf("pipeline %s: IR cannot topologically order graph", name)
		}
		for _, id := range plan.Order {
			n := p.Nodes[id]
			resultType := PortAny
			switch n.Kind {
			case "condition", "policy.check":
				resultType = PortBool
			case "input":
				resultType = PortObject
			}
			compiled := &NodeIR{Name: id, Kind: n.Kind, Span: n.Span, Output: resultType, Expressions: map[string]*expr.Value{}}
			for _, attr := range []string{"value", "mapping", "arguments", "test"} {
				raw, ok := n.Attrs[attr]
				if !ok {
					continue
				}
				var expected *cel.Type
				if attr == "test" {
					expected = cel.BoolType
				}
				if attr == "arguments" {
					expected = cel.DynType
				}
				value, err := expr.CompileValue(raw, expected)
				if err != nil {
					return fmt.Errorf("pipeline %s node %s %s at line %d: %w", name, id, attr, n.Span.Line, err)
				}
				compiled.Expressions[attr] = value
				refs := extractRefs(raw)
				for _, dep := range refs {
					if _, ok := p.Nodes[dep]; !ok {
						return fmt.Errorf("pipeline %s node %s: expression references missing node %s", name, id, dep)
					}
					if dep == id || !isAncestor(dep, id, p.Edges) {
						return fmt.Errorf("pipeline %s node %s: %s is not a control predecessor (data may be unavailable)", name, id, dep)
					}
					compiled.Dependencies = append(compiled.Dependencies, dep)
				}
			}
			sort.Strings(compiled.Dependencies)
			compiled.Dependencies = uniqueStrings(compiled.Dependencies)
			plan.Nodes[id] = compiled
		}
		plans[name] = plan
	}
	c.Plans = plans
	return nil
}
func extractRefs(value any) []string {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "literal:") {
			return nil
		}
		refs := []string{}
		for _, m := range nodeRef.FindAllStringSubmatch(stripCELStrings(v), -1) {
			refs = append(refs, m[1])
		}
		return refs
	case map[string]any:
		refs := []string{}
		for _, item := range v {
			refs = append(refs, extractRefs(item)...)
		}
		return refs
	case []any:
		refs := []string{}
		for _, item := range v {
			refs = append(refs, extractRefs(item)...)
		}
		return refs
	default:
		return nil
	}
}
func isAncestor(from, to string, edges []Edge) bool {
	visited := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		if id == to {
			return true
		}
		if visited[id] {
			return false
		}
		visited[id] = true
		for _, e := range edges {
			if e.From == id && visit(e.To) {
				return true
			}
		}
		return false
	}
	return visit(from)
}
func uniqueStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	dst := in[:1]
	for _, s := range in[1:] {
		if s != dst[len(dst)-1] {
			dst = append(dst, s)
		}
	}
	return dst
}

// stripCELStrings masks quoted literal spans before extracting static node
// references, so a message such as 'nodes.missing' is not mistaken for a
// dependency. Dynamic subscript expressions remain runtime-checked.
func stripCELStrings(s string) string {
	b := []byte(s)
	var quote byte
	escaped := false
	for i := range b {
		c := b[i]
		if quote != 0 {
			if escaped {
				escaped = false
				b[i] = ' '
				continue
			}
			if c == '\\' {
				escaped = true
				b[i] = ' '
				continue
			}
			if c == quote {
				quote = 0
			}
			b[i] = ' '
		} else if c == '\'' || c == '"' {
			quote = c
			b[i] = ' '
		}
	}
	return string(b)
}
