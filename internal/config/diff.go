package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

type SemanticChange struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Before    any    `json:"before,omitempty"`
	After     any    `json:"after,omitempty"`
}

// SemanticDiff compares executable configuration values, not HCL formatting,
// comments, source spans, or derived execution programs. Diffs are sorted and
// stable, including when Go map iteration order changes.
func SemanticDiff(a, b *Config) ([]SemanticChange, error) {
	av, err := semanticValue(a)
	if err != nil {
		return nil, err
	}
	bv, err := semanticValue(b)
	if err != nil {
		return nil, err
	}
	changes := []SemanticChange{}
	compareSemantic("", av, bv, &changes)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}
func semanticValue(cfg *Config) (any, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}
func compareSemantic(path string, a, b any, changes *[]SemanticChange) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]struct{}{}
		for k := range am {
			keys[k] = struct{}{}
		}
		for k := range bm {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			sub := k
			if path != "" {
				sub = path + "." + k
			}
			left, hasLeft := am[k]
			right, hasRight := bm[k]
			if !hasLeft {
				*changes = append(*changes, SemanticChange{Path: sub, Operation: "added", After: right})
			} else if !hasRight {
				*changes = append(*changes, SemanticChange{Path: sub, Operation: "removed", Before: left})
			} else {
				compareSemantic(sub, left, right, changes)
			}
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*changes = append(*changes, SemanticChange{Path: path, Operation: "changed", Before: a, After: b})
	}
}
func (c *Config) SemanticDigest() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("semantic digest: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(raw)), nil
}
