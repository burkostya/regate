package config

import (
	"fmt"
	"sort"
)

const MaxPurePipelineCallDepth = 8

// validatePurePipelineCalls bounds recursion and prevents any invoked
// subpipeline from acquiring hidden effects or suspension points. A child may
// call another pure child, but arbitrary effectful nesting requires a durable,
// hierarchical effect identity and checkpoint contract first.
func (c *Config) validatePurePipelineCalls() error {
	pipelineNames := make([]string, 0, len(c.Pipelines))
	for name := range c.Pipelines { pipelineNames = append(pipelineNames, name) }
	sort.Strings(pipelineNames)

	var check func(string, int, bool, map[string]bool) error
	check = func(name string, depth int, pure bool, stack map[string]bool) error {
		if depth > MaxPurePipelineCallDepth {
			return fmt.Errorf("pipeline %s: nested call depth exceeds %d", name, MaxPurePipelineCallDepth)
		}
		if stack[name] {
			return fmt.Errorf("pipeline %s: cyclic pipeline.call dependency", name)
		}
		stack[name] = true
		defer delete(stack, name)

		pipeline := c.Pipelines[name]
		nodeNames := make([]string, 0, len(pipeline.Nodes))
		for n := range pipeline.Nodes { nodeNames = append(nodeNames, n) }
		sort.Strings(nodeNames)
		if pure {
			if pipeline.Nodes[pipeline.Entry].Kind != "input" {
				return fmt.Errorf("pipeline %s: called pure pipeline entry must be an input node", name)
			}
			if outputNodes(pipeline) != 1 {
				return fmt.Errorf("pipeline %s: called pure pipeline needs exactly one output node", name)
			}
		}
		for _, nodeName := range nodeNames {
			node := pipeline.Nodes[nodeName]
			if pure && !pureCompositionNode(node.Kind) {
				return fmt.Errorf("pipeline %s node %s: %s cannot execute inside a pure pipeline.call", name, nodeName, node.Kind)
			}
			if node.Kind != "pipeline.call" { continue }
			childName := node.Str("pipeline")
			child, ok := c.Pipelines[childName]
			if !ok {
				return fmt.Errorf("pipeline %s node %s: unknown called pipeline %s", name, nodeName, childName)
			}
			if child.Nodes[child.Entry].Kind != "input" {
				return fmt.Errorf("pipeline %s node %s: called pipeline %s needs input entry", name, nodeName, childName)
			}
			if outputNodes(child) != 1 {
				return fmt.Errorf("pipeline %s node %s: called pipeline %s needs exactly one output", name, nodeName, childName)
			}
			if err := check(childName, depth+1, true, stack); err != nil {
				return fmt.Errorf("pipeline %s node %s: %w", name, nodeName, err)
			}
		}
		return nil
	}
	for _, name := range pipelineNames {
		if err := check(name, 1, false, map[string]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func pureCompositionNode(kind string) bool {
	switch kind {
	case "input", "output", "transform", "filter", "validate", "condition",
		"parallel", "join", "map", "mask.pii", "pipeline.call":
		return true
	default:
		return false
	}
}

func outputNodes(p Pipeline) int {
	n := 0
	for _, node := range p.Nodes {
		if node.Kind == "output" { n++ }
	}
	return n
}
