package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ReplaceBlock changes exactly one top-level block. Every byte outside the
// replaced range, including comments, whitespace and opaque fragments, is
// preserved. Invalid/ambiguous replacements are rejected atomically.
func ReplaceBlock(source, typ, label, replacement string) (string, error) {
	blocks, err := Parse(source)
	if err != nil {
		return "", err
	}
	var found *Block
	for _, b := range blocks {
		if b.Type == typ && len(b.Labels) == 1 && b.Labels[0] == label {
			if found != nil {
				return "", fmt.Errorf("ambiguous %s %q", typ, label)
			}
			found = b
		}
	}
	if found == nil {
		return "", fmt.Errorf("missing %s %q", typ, label)
	}
	nb, err := Parse(replacement)
	if err != nil {
		return "", fmt.Errorf("replacement: %w", err)
	}
	if len(nb) != 1 || nb[0].Type != typ || len(nb[0].Labels) != 1 || nb[0].Labels[0] != label {
		return "", fmt.Errorf("replacement must contain only %s %q", typ, label)
	}
	next := source[:found.Start] + replacement + source[found.End:]
	// If the graph retains the same labeled blocks/attributes, change only
	// differing expressions. This preserves comments and unsupported editor
	// decorations *inside* the target pipeline, not merely outside it.
	if edits, compatible := expressionEdits(found, nb[0], replacement); compatible {
		next = source
		sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
		for _, edit := range edits {
			next = next[:edit.start] + edit.value + next[edit.end:]
		}
	}
	if _, err := Decode(next); err != nil {
		return "", err
	}
	return next, nil
}

type BlockSelector struct {
	Type  string
	Label string
}

// SetAttribute edits only an existing attribute expression. It never rewrites
// nearby comments, blocks, or unknown source syntax. Callers must provide an
// explicit block path, e.g. pipeline:summary / node:fetch. New attributes can
// be introduced via ReplaceBlock after validating the intended whole block.
func SetAttribute(source string, path []BlockSelector, name, expression string) (string, error) {
	if len(path) == 0 {
		return "", fmt.Errorf("empty block path")
	}
	parsed, err := Parse(source)
	if err != nil {
		return "", err
	}
	blocks := parsed
	var target *Block
	for _, sel := range path {
		target = nil
		for _, b := range blocks {
			if b.Type == sel.Type && len(b.Labels) == 1 && b.Labels[0] == sel.Label {
				if target != nil {
					return "", fmt.Errorf("ambiguous %s %q", sel.Type, sel.Label)
				}
				target = b
			}
		}
		if target == nil {
			return "", fmt.Errorf("missing %s %q", sel.Type, sel.Label)
		}
		blocks = target.Children
	}
	r, ok := target.AttrRanges[name]
	if !ok {
		return "", fmt.Errorf("attribute %q absent; use block replacement to add", name)
	}
	if strings.TrimSpace(expression) == "" {
		return "", fmt.Errorf("empty attribute expression")
	}
	_, diags := hclsyntax.ParseExpression([]byte(expression), "patch.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return "", fmt.Errorf("replacement expression: %s", diags.Error())
	}
	next := source[:r.Start] + expression + source[r.End:]
	if _, err := Decode(next); err != nil {
		return "", err
	}
	return next, nil
}

type expressionEdit struct {
	start, end int
	value      string
}

// expressionEdits returns an all-or-nothing patch set for structurally
// equivalent HCL blocks. Structural edits fall back to whole-block
// replacement; callers should use attribute_patch where possible.
func expressionEdits(old, desired *Block, replacement string) ([]expressionEdit, bool) {
	if old.Type != desired.Type || !reflect.DeepEqual(old.Labels, desired.Labels) || len(old.Attrs) != len(desired.Attrs) || len(old.Children) != len(desired.Children) {
		return nil, false
	}
	edits := []expressionEdit{}
	for key, newValue := range desired.Attrs {
		oldValue, ok := old.Attrs[key]
		if !ok {
			return nil, false
		}
		if reflect.DeepEqual(oldValue, newValue) {
			continue
		}
		left, okLeft := old.AttrRanges[key]
		right, okRight := desired.AttrRanges[key]
		if !okLeft || !okRight {
			return nil, false
		}
		edits = append(edits, expressionEdit{start: left.Start, end: left.End, value: replacement[right.Start:right.End]})
	}
	for i, newChild := range desired.Children {
		nested, ok := expressionEdits(old.Children[i], newChild, replacement)
		if !ok {
			return nil, false
		}
		edits = append(edits, nested...)
	}
	return edits, true
}
