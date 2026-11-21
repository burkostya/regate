package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSchemaFingerprints fingerprints the SDK-decoded input and output
// schemas. Duplicate names are ambiguous even when the descriptors agree.
func ToolSchemaFingerprints(tools []map[string]any) map[string]string {
	pins := make(map[string]string)
	seen := make(map[string]bool)
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		if name == "" || seen[name] {
			delete(pins, name)
			continue
		}
		seen[name] = true
		if pin := toolSchemaFingerprint(tool["inputSchema"], tool["outputSchema"]); pin != "" {
			pins[name] = pin
		}
	}
	return pins
}

func toolSchemaFingerprint(inputSchema, outputSchema any) string {
	input, ok := inputSchema.(map[string]any)
	if !ok || input == nil {
		return ""
	}
	output, ok := outputSchema.(map[string]any)
	if !ok && outputSchema != nil {
		return ""
	}
	schemas := struct {
		Input  map[string]any `json:"inputSchema"`
		Output map[string]any `json:"outputSchema"`
	}{Input: input, Output: output}
	body, err := json.Marshal(schemas)
	if err != nil {
		return ""
	}
	fingerprint := sha256.Sum256(body)
	return hex.EncodeToString(fingerprint[:])
}

func verifyToolSchemaPin(ctx context.Context, session *sdk.ClientSession, name, pin string) error {
	var matched string
	var count int
	err := visitCatalog("tools/list", toolPages(ctx, session), func(tool *sdk.Tool) error {
		if tool == nil {
			return errors.New("tools/list returned a null entry")
		}
		if tool.Name == name {
			count++
			if count == 1 {
				matched = toolSchemaFingerprint(tool.InputSchema, tool.OutputSchema)
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, sdk.ErrSessionMissing) {
			return ErrSessionExpired
		}
		return errors.New("could not verify upstream tool schema")
	}
	if count != 1 || matched != pin {
		return errors.New("upstream tool schema does not match the configured pin")
	}
	return nil
}
