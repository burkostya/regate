package engine

import "fmt"

// validateMCPToolOutcome is used at pipeline and hook boundaries, not at
// transparent MCP proxy surfaces. MCP tools/call may succeed at transport
// level while the tool itself reports isError=true; such a result must not
// satisfy a data contract, authorize a Decision, or activate a write node.
// A confirmed external write may still have occurred; the effect journal
// remains authoritative and must not be erased or automatically replayed.
func validateMCPToolOutcome(raw map[string]any) error {
	if raw == nil { return fmt.Errorf("upstream MCP tool response missing") }
	if flag, exists := raw["isError"]; exists {
		isError, ok := flag.(bool)
		if !ok { return fmt.Errorf("upstream MCP tool returned malformed isError flag") }
		if isError { return fmt.Errorf("upstream MCP tool reported isError=true") }
	}
	return nil
}
