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
	InputSchema  *SchemaContract
	OutputSchema *SchemaContract
	InputPorts   map[string]*SchemaContract // Named JSON Schema bindings supplied only by data edges.
}
type ToolContract struct {
	Input  *SchemaContract
	Output *SchemaContract
}

type PipelineIR struct {
	Name  string
	Entry string
	Order []string // Deterministic topological order.
	Nodes map[string]*NodeIR
}

var nodeRef = regexp.MustCompile(`\bnodes\.([a-zA-Z_][a-zA-Z0-9_]*)`)
var portRef = regexp.MustCompile(`\bports\.([a-zA-Z_][a-zA-Z0-9_]*)`)
var portIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func (c *Config) Compile() error {
	plans := map[string]*PipelineIR{}
	toolContracts := map[string]map[string]ToolContract{}
	for surfaceName, surface := range c.Surfaces {
		toolContracts[surfaceName] = map[string]ToolContract{}
		for _, tool := range surface.Tools {
			input, err := compileSchema(tool.InputSchema)
			if err != nil {
				return fmt.Errorf("surface %s tool %s input_schema: %w", surfaceName, tool.Name, err)
			}
			output, err := compileSchema(tool.OutputSchema)
			if err != nil {
				return fmt.Errorf("surface %s tool %s output_schema: %w", surfaceName, tool.Name, err)
			}
			toolContracts[surfaceName][tool.Name] = ToolContract{Input: input, Output: output}
		}
	}

	for name, p := range c.Pipelines {
		plan := &PipelineIR{Name: name, Entry: p.Entry, Nodes: map[string]*NodeIR{}}
		forward := map[string][]string{}
		incoming := map[string]int{}
		for id := range p.Nodes {
			incoming[id] = 0
		}
		for _, e := range p.Edges {
			if e.Kind == "data" { continue }
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
		guaranteed := guaranteedCompleted(p, plan.Order)
		for _, id := range plan.Order {
			n := p.Nodes[id]
			resultType := PortAny
			switch n.Kind {
			case "condition", "policy.check":
				resultType = PortBool
			case "input":
				resultType = PortObject
			}
			compiled := &NodeIR{Name: id, Kind: n.Kind, Span: n.Span, Output: resultType, Expressions: map[string]*expr.Value{}, InputPorts: map[string]*SchemaContract{}}
			if portSchemas, ok := n.Attrs["input_ports"].(map[string]any); ok {
				for portName, raw := range portSchemas {
					declared, ok := raw.(map[string]any)
					if !ok { return fmt.Errorf("pipeline %s node %s port %s: schema must be an object", name, id, portName) }
					schema, err := compileSchema(declared)
					if err != nil { return fmt.Errorf("pipeline %s node %s port %s: %w", name, id, portName, err) }
					compiled.InputPorts[portName] = schema
				}
			}
			if raw, ok := n.Attrs["input_schema"].(map[string]any); ok {
				schema, err := compileSchema(raw)
				if err != nil {
					return fmt.Errorf("pipeline %s node %s input_schema: %w", name, id, err)
				}
				compiled.InputSchema = schema
			}
			if raw, ok := n.Attrs["output_schema"].(map[string]any); ok {
				schema, err := compileSchema(raw)
				if err != nil {
					return fmt.Errorf("pipeline %s node %s output_schema: %w", name, id, err)
				}
				compiled.OutputSchema = schema
			}
			if compiled.OutputSchema != nil {
				if types := compiled.OutputSchema.Types(); len(types) == 1 {
					switch types[0] {
					case "object":
						compiled.Output = PortObject
					case "boolean":
						compiled.Output = PortBool
					}
				}
			}
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
				for _, portName := range extractPortRefs(raw) {
					if _, ok := compiled.InputPorts[portName]; !ok {
						return fmt.Errorf("pipeline %s node %s: unknown input port %s", name, id, portName)
					}
				}
				refs := extractRefs(raw)
				for _, dep := range refs {
					if _, ok := p.Nodes[dep]; !ok {
						return fmt.Errorf("pipeline %s node %s: expression references missing node %s", name, id, dep)
					}
					if dep == id || !isAncestor(dep, id, p.Edges) {
						return fmt.Errorf("pipeline %s node %s: %s is not a control predecessor (data may be unavailable)", name, id, dep)
					}
					if !guaranteed[id][dep] {
						return fmt.Errorf("pipeline %s node %s: expression predecessor %s is not guaranteed to complete before activation", name, id, dep)
					}
					compiled.Dependencies = append(compiled.Dependencies, dep)
				}
			}
			for _, edge := range p.Edges {
				if edge.Kind == "data" && edge.To == id {
					compiled.Dependencies = append(compiled.Dependencies, edge.From)
				}
			}
			sort.Strings(compiled.Dependencies)
			compiled.Dependencies = uniqueStrings(compiled.Dependencies)
			plan.Nodes[id] = compiled
		}
		// If a node directly consumes a declared predecessor's output,
		// reject provably incompatible types. Uncertain paths are refined
		// at execution time before any external side effect.
		for _, id := range plan.Order {
			node := p.Nodes[id]
			consumer := plan.Nodes[id]
			if consumer.InputSchema == nil {
				continue
			}
			attr := "value"
			if node.Kind == "mcp.call" {
				attr = "arguments"
			}
			if text, ok := node.Attrs[attr].(string); ok {
				if parent, ok := schemaRefFromExpression(text); ok {
					source := plan.Nodes[parent]
					if source != nil {
						if err := CheckKnownCompatibility(source.OutputSchema, consumer.InputSchema); err != nil {
							return fmt.Errorf("pipeline %s %s -> %s: %w", name, parent, id, err)
						}
					}
				}
			}
		}
		for _, edge := range p.Edges {
			if edge.Kind != "data" { continue }
			if !guaranteed[edge.To][edge.From] {
				return fmt.Errorf("pipeline %s node %s: data port %s source %s is not guaranteed to complete before activation", name, edge.To, edge.ToPort, edge.From)
			}
			producer := plan.Nodes[edge.From].OutputSchema
			consumer := plan.Nodes[edge.To].InputPorts[edge.ToPort]
			if err := CheckKnownCompatibility(producer, consumer); err != nil {
				return fmt.Errorf("pipeline %s data port %s.%s -> %s.%s: %w", name, edge.From, "value", edge.To, edge.ToPort, err)
			}
		}
		plans[name] = plan
	}
	// Compare each tool's published output contract with declared outputs.
	for surfaceName, surface := range c.Surfaces {
		for _, tool := range surface.Tools {
			published := toolContracts[surfaceName][tool.Name].Output
			if tool.Pipeline == "" || published == nil {
				continue
			}
			p := c.Pipelines[tool.Pipeline]
			for _, node := range p.Nodes {
				if node.Kind != "output" {
					continue
				}
				if err := CheckKnownCompatibility(plans[p.Name].Nodes[node.Name].OutputSchema, published); err != nil {
					return fmt.Errorf("surface %s tool %s output %s: %w", surfaceName, tool.Name, node.Name, err)
				}
			}
		}
	}
	c.Plans = plans
	c.ToolContracts = toolContracts
	return nil
}
// extractPortRefs examines statically named CEL port references. Dynamic
// indexing stays bounded by the declared, runtime-validated port map.
func extractPortRefs(value any) []string {
	switch v := value.(type) {
	case string:
		if strings.HasPrefix(v, "literal:") { return nil }
		refs := []string{}
		for _, match := range portRef.FindAllStringSubmatch(stripCELStrings(v), -1) {
			refs = append(refs, match[1])
		}
		return refs
	case map[string]any:
		refs := []string{}
		for _, item := range v { refs = append(refs, extractPortRefs(item)...)}
		return refs
	case []any:
		refs := []string{}
		for _, item := range v { refs = append(refs, extractPortRefs(item)...)}
		return refs
	default:
		return nil
	}
}
// guaranteedCompleted computes the nodes that must have COMPLETED whenever
// the target node activates. A join with threshold k of n predecessors may
// activate after any k distinct fired edges; a source is guaranteed only when
// it appears in at least n-k+1 predecessor guarantee sets. This is the
// intersection of the unions across all possible winning k-subsets.
// Skipped, guarded and suspended branches cannot invent a completed source.
// Runtime and compiler use the same join thresholds; invalid configurations
// are rejected by Validate before this function is called.
func guaranteedCompleted(p Pipeline, order []string) map[string]map[string]bool {
	incoming := map[string][]string{}
	for _, edge := range p.Edges {
		if edge.Kind != "data" {
			incoming[edge.To] = append(incoming[edge.To], edge.From)
		}
	}
	guaranteed := map[string]map[string]bool{}
	for _, nodeName := range order {
		set := map[string]bool{nodeName: true}
		if nodeName == p.Entry {
			guaranteed[nodeName] = set
			continue
		}
		parents := incoming[nodeName]
		if len(parents) == 0 {
			guaranteed[nodeName] = set
			continue
		}
		threshold := 1 // Ordinary graph merges activate on any fired edge.
		node := p.Nodes[nodeName]
		if node.Kind == "join" {
			switch node.Str("mode") {
			case "", "all":
				threshold = len(parents)
			case "any":
				threshold = 1
			case "quorum":
				threshold = node.Int("quorum", 0)
			}
		}
		frequency := map[string]int{}
		for _, parent := range parents {
			for predecessor := range guaranteed[parent] {
				frequency[predecessor]++
			}
		}
		for predecessor, count := range frequency {
			if count >= len(parents)-threshold+1 {
				set[predecessor] = true
			}
		}
		guaranteed[nodeName] = set
	}
	return guaranteed
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
			if e.Kind != "data" && e.From == id && visit(e.To) {
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
