package engine

import "fmt"

// validateMCPToolOutcome is used at pipeline and hook boundaries, not at
// transparent MCP proxy surfaces. MCP tools/call may succeed at transport
// level while the tool itself reports isError=true; such a result must not
// satisfy a data contract, authorize a Decision, or activate a write node.
// An external write may have committed even when the tool reports an error.
// For writes, the journal remains UNKNOWN until a verified readback or
// deliberate operator decision, and it must never be automatically replayed.
func validateMCPToolOutcome(raw map[string]any) error {
	if raw == nil { return fmt.Errorf("upstream MCP tool response missing") }
	if flag, exists := raw["isError"]; exists {
		isError, ok := flag.(bool)
		if !ok { return fmt.Errorf("upstream MCP tool returned malformed isError flag") }
		if isError { return fmt.Errorf("upstream MCP tool reported isError=true") }
	}
	return nil
}
