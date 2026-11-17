package config

import (
	"fmt"
	"sort"
)

const MaxPurePipelineCallDepth = 8

// validatePipelineCalls bounds recursion and requires explicit operator
// opt-in for effectful children and for nested checkpoint suspension.
// Every enclosing call must explicitly permit suspension; a pure call may
// not transitively delegate either effects or suspension.
func (c *Config) validatePipelineCalls() error {
	pipelineNames := make([]string, 0, len(c.Pipelines))
	for name := range c.Pipelines { pipelineNames = append(pipelineNames, name) }
	sort.Strings(pipelineNames)

	var check func(string, int, bool, bool, map[string]bool) error
	check = func(name string, depth int, pure, allowSuspend bool, stack map[string]bool) error {
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
		if depth > 1 {
			if pipeline.Nodes[pipeline.Entry].Kind != "input" {
				return fmt.Errorf("pipeline %s: called pipeline entry must be an input node", name)
			}
			if outputNodes(pipeline) < 1 {
				return fmt.Errorf("pipeline %s: called pipeline needs at least one output node", name)
			}
		}
		for _, nodeName := range nodeNames {
			node := pipeline.Nodes[nodeName]
			if depth > 1 && node.Kind == "suspend" && !allowSuspend {
				return fmt.Errorf("pipeline %s node %s: suspend requires durable nested checkpoint support (allow_suspend=true on every enclosing effectful pipeline.call)", name, nodeName)
			}
			if pure && !pureCompositionNode(node.Kind) {
				return fmt.Errorf("pipeline %s node %s: %s cannot execute inside a pure pipeline.call", name, nodeName, node.Kind)
			}
			if pure && node.Kind == "pipeline.call" && node.Attrs["allow_effects"] == true {
				return fmt.Errorf("pipeline %s node %s: pure pipeline.call cannot delegate effects", name, nodeName)
			}
			if node.Kind == "map" && node.Str("pipeline") != "" {
				if pure && node.Attrs["allow_effects"] == true {
					return fmt.Errorf("pipeline %s node %s: pure parent cannot delegate effects through map", name, nodeName)
				}
				childName := node.Str("pipeline")
				child, ok := c.Pipelines[childName]
				if !ok {
					return fmt.Errorf("pipeline %s node %s: unknown mapped pipeline %s", name, nodeName, childName)
				}
				if child.Nodes[child.Entry].Kind != "input" || outputNodes(child) != 1 {
					return fmt.Errorf("pipeline %s node %s: map child needs an input entry and exactly one output node", name, nodeName)
				}
				if node.Attrs["allow_effects"] == true {
					terminalName := namedOutputNodes(child)[0]
					if _, ok := child.Nodes[terminalName].Attrs["output_schema"].(map[string]any); !ok {
						return fmt.Errorf("pipeline %s node %s: effectful map child output %s requires output_schema", name, nodeName, terminalName)
					}
				}
				childPure := pure || node.Attrs["allow_effects"] != true
				// A mapped child can never suspend: there is currently no
				// durable per-item continuation/checkpoint protocol.
				if err := check(childName, depth+1, childPure, false, stack); err != nil {
					return fmt.Errorf("pipeline %s node %s: %w", name, nodeName, err)
				}
				continue
			}
			if node.Kind != "pipeline.call" { continue }
			if node.Attrs["allow_suspend"] == true && node.Attrs["allow_effects"] != true {
				return fmt.Errorf("pipeline %s node %s: allow_suspend requires allow_effects=true", name, nodeName)
			}
			childName := node.Str("pipeline")
			child, ok := c.Pipelines[childName]
			if !ok {
				return fmt.Errorf("pipeline %s node %s: unknown called pipeline %s", name, nodeName, childName)
			}
			if child.Nodes[child.Entry].Kind != "input" {
				return fmt.Errorf("pipeline %s node %s: called pipeline %s needs input entry", name, nodeName, childName)
			}
			outputs := namedOutputNodes(child)
			if len(outputs) == 0 {
				return fmt.Errorf("pipeline %s node %s: called pipeline %s needs at least one output", name, nodeName, childName)
			}
			if len(outputs) > 1 {
				ports, _ := node.Attrs["output_ports"].(map[string]any)
				for _, outputName := range outputs {
					if outputName == "value" {
						return fmt.Errorf("pipeline %s node %s: multi-output child cannot name a terminal value", name, nodeName)
					}
					if _, ok := ports[outputName]; !ok {
						return fmt.Errorf("pipeline %s node %s: must declare output_ports for child terminal %s", name, nodeName, outputName)
					}
				}
				existing := make(map[string]bool, len(outputs))
				for _, outputName := range outputs { existing[outputName] = true }
				portNames := make([]string, 0, len(ports))
				for portName := range ports { portNames = append(portNames, portName) }
				sort.Strings(portNames)
				for _, portName := range portNames {
					if portName != "value" && !existing[portName] {
						return fmt.Errorf("pipeline %s node %s: undeclared child terminal %s in output_ports", name, nodeName, portName)
					}
				}
			}
			childPure := pure || node.Attrs["allow_effects"] != true
			childAllowSuspend := allowSuspend && node.Attrs["allow_suspend"] == true
			if err := check(childName, depth+1, childPure, childAllowSuspend, stack); err != nil {
				return fmt.Errorf("pipeline %s node %s: %w", name, nodeName, err)
			}
		}
		return nil
	}
	for _, name := range pipelineNames {
		if err := check(name, 1, false, true, map[string]bool{}); err != nil {
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

// namedOutputNodes returns stable terminal identities for multi-output calls.
func namedOutputNodes(p Pipeline) []string {
	names := []string{}
	for name, node := range p.Nodes {
		if node.Kind == "output" { names = append(names, name) }
	}
	sort.Strings(names)
	return names
}

func outputNodes(p Pipeline) int {
	n := 0
	for _, node := range p.Nodes {
		if node.Kind == "output" { n++ }
	}
	return n
}
