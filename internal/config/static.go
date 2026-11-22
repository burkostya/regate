package config

import (
	"fmt"
	"strings"
)

// MergeStaticConfig replaces the deployed prefix and preserves the remaining
// HCL bytes. An identical block can move from the dynamic source to the file.
func MergeStaticConfig(dynamic, static string) (source, prefix string, err error) {
	if static == "" {
		return dynamic, "", nil
	}
	declared, err := Parse(static)
	if err != nil {
		return "", "", fmt.Errorf("static HCL could not be parsed")
	}
	blocks, err := ParseForEdit(dynamic)
	if err != nil {
		return "", "", fmt.Errorf("dynamic HCL could not be parsed")
	}
	staticBlocks := make(map[string]string, len(declared))
	for _, block := range declared {
		key := fmt.Sprintf("%s %q", block.Type, block.Labels)
		if _, exists := staticBlocks[key]; exists {
			return "", "", fmt.Errorf("duplicate block in static HCL")
		}
		staticBlocks[key] = static[block.Start:block.End]
	}
	prefix = static + "\n\n"
	var merged strings.Builder
	merged.WriteString(prefix)
	offset := 0
	for _, block := range blocks {
		key := fmt.Sprintf("%s %q", block.Type, block.Labels)
		candidate, exists := staticBlocks[key]
		if !exists {
			continue
		}
		if candidate != dynamic[block.Start:block.End] {
			return "", "", fmt.Errorf("static block differs from its dynamic counterpart; copy the active block to promote it")
		}
		delete(staticBlocks, key)
		merged.WriteString(dynamic[offset:block.Start])
		offset = block.End
	}
	merged.WriteString(dynamic[offset:])
	return merged.String(), prefix, nil
}
