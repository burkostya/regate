package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// ReplaceBlock performs a source-preserving structural merge of a named HCL
// block. Matching nodes and edges are retained byte-for-byte except for their
// changed expressions. Insertions and removals affect only the named element;
// neighboring comments and all opaque source outside the edit are preserved.
// Ambiguous matching is deliberately rejected rather than overwriting data.
func ReplaceBlock(source, typ, label, replacement string) (string, error) {
	blocks, err := ParseForEdit(source)
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
	desired, err := ParseForEdit(replacement)
	if err != nil {
		return "", fmt.Errorf("replacement: %w", err)
	}
	if len(desired) != 1 || desired[0].Type != typ || len(desired[0].Labels) != 1 || desired[0].Labels[0] != label {
		return "", fmt.Errorf("replacement must contain only %s %q", typ, label)
	}
	edits, err := structuralEdits(source, replacement, found, desired[0])
	if err != nil {
		return "", err
	}
	next, err := applyEdits(source, edits)
	if err != nil {
		return "", err
	}
	if err = validateSourceEdit(source, next); err != nil {
        return "", err
    }
	return next, nil
}

type BlockSelector struct {
	Type  string
	Label string
}

// knownEditableAttr checks the compiler's closed executable schema. Unknown
// fields may be retained in drafts but cannot be silently changed by a graph
// editor that does not understand their semantics.
func knownEditableAttr(b *Block, name string) bool {
    if b.Type == "node" {
        // Operators must be able to repair an unresolved/unknown kind in an
        // incomplete draft without switching to the full-source editor.
        if name == "kind" || name == "input_schema" || name == "output_schema" { return true }
        _, ok := nodeAttributes[b.String("kind")][name]
        return ok
    }
    _, ok := blockAttributes[b.Type][name]
    return ok
}

// validateSourceEdit is fail-closed for known-good executable configurations.
// Invalid drafts are allowed to stay invalid so operators can edit opaque HCL,
// but activation always calls Decode and will reject them.
func validateSourceEdit(original, edited string) error {
    if _, err := ParseForEdit(edited); err != nil { return err }
    if _, err := Decode(original); err == nil {
        if _, err := Decode(edited); err != nil { return err }
    }
    return nil
}

// SetAttribute replaces only the HCL value expression of an existing field.
func SetAttribute(source string, path []BlockSelector, name, expression string) (string, error) {
	target, err := selectBlock(source, path)
	if err != nil {
		return "", err
	}
	r, ok := target.AttrRanges[name]
	if !ok {
		return "", fmt.Errorf("attribute %q absent; use AddAttribute", name)
	}
    if !knownEditableAttr(target,name) {
        return "", fmt.Errorf("attribute %q is opaque; edit it in the HCL source, not the graph", name)
    }
	if err = validateExpression(expression); err != nil {
		return "", err
	}
	next, err := applyEdits(source, []sourceEdit{{start: r.Start, end: r.End, value: expression}})
	if err != nil {
		return "", err
	}
	if err = validateSourceEdit(source, next); err != nil {
        return "", err
    }
	return next, nil
}

// AddAttribute inserts one new field without reformatting any existing source.
// It refuses a duplicate field and validates the entire candidate graph.
func AddAttribute(source string, path []BlockSelector, name, expression string) (string, error) {
	target, err := selectBlock(source, path)
	if err != nil {
		return "", err
	}
	if _, exists := target.Attrs[name]; exists {
		return "", fmt.Errorf("attribute %q already exists", name)
	}
	if err = validateExpression(expression); err != nil {
		return "", err
	}
	if !hclsyntax.ValidIdentifier(name) {
		return "", fmt.Errorf("invalid HCL attribute name %q", name)
	}
    if !knownEditableAttr(target,name) {
        return "", fmt.Errorf("cannot insert unsupported attribute %q with a graph patch", name)
    }
	indent := blockChildIndent(source, target)
	insert := fmt.Sprintf("%s%s = %s\n", indent, name, expression)
	next, err := insertBeforeClose(source, target, insert)
	if err != nil {
		return "", err
	}
	if err = validateSourceEdit(source, next); err != nil {
        return "", err
    }
	return next, nil
}

func validateExpression(expression string) error {
	if strings.TrimSpace(expression) == "" {
		return fmt.Errorf("empty attribute expression")
	}
	_, diag := hclsyntax.ParseExpression([]byte(expression), "patch.hcl", hcl.Pos{Line: 1, Column: 1})
	if diag.HasErrors() {
		return fmt.Errorf("replacement expression: %s", diag.Error())
	}
	return nil
}

func selectBlock(source string, path []BlockSelector) (*Block, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("empty block path")
	}
	blocks, err := ParseForEdit(source)
	if err != nil {
		return nil, err
	}
	var selected *Block
	for _, sel := range path {
		selected = nil
		for _, b := range blocks {
			if b.Type == sel.Type && len(b.Labels) == 1 && b.Labels[0] == sel.Label {
				if selected != nil {
					return nil, fmt.Errorf("ambiguous %s %q", sel.Type, sel.Label)
				}
				selected = b
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("missing %s %q", sel.Type, sel.Label)
		}
		blocks = selected.Children
	}
	return selected, nil
}

type sourceEdit struct {
	start, end int
	value      string
}

// Identity is independent of a node's position. Edges, which do not have HCL
// labels, are identified by the two endpoints and the guard; duplicate edges
// are ambiguous and cannot be structurally edited safely.
func childIdentity(b *Block) (string, error) {
	if len(b.Labels) > 0 {
		return b.Type + "/" + strings.Join(b.Labels, "/"), nil
	}
	if b.Type == "edge" {
		from, to := b.String("from"), b.String("to")
		if from == "" || to == "" {
			return "", fmt.Errorf("unidentifiable edge at line %d", b.Line)
		}
		return "edge/" + from + "/" + to + "/" + b.String("when"), nil
	}
	return "", fmt.Errorf("unidentifiable %s at line %d", b.Type, b.Line)
}
func knownNestedChild(parent, child string) bool { return nestedBlocks[parent][child] }

// requireOpaqueFragmentsUnchanged ensures a structural patch never silently
// rewrites or adds unknown nested HCL. An unrepresented opaque child is kept;
// if the editor explicitly includes one, it must match an existing fragment.
func requireOpaqueFragmentsUnchanged(original, replacement string, old, desired *Block) error {
    kept := map[string]int{}
    for _, child := range old.Children {
        if !knownNestedChild(old.Type, child.Type) {
            kept[original[child.Start:child.End]]++
        }
    }
    for _, child := range desired.Children {
        if knownNestedChild(desired.Type, child.Type) { continue }
        fragment := replacement[child.Start:child.End]
        if kept[fragment] == 0 {
            return fmt.Errorf("opaque %s cannot be added or modified through structural graph patch", child.Type)
        }
        kept[fragment]--
    }
    return nil
}

func indexedChildren(b *Block) (map[string]*Block, error) {
	indexed := map[string]*Block{}
	for _, child := range b.Children {
        if !knownNestedChild(b.Type, child.Type) { continue }
		key, err := childIdentity(child)
		if err != nil {
			return nil, err
		}
		if indexed[key] != nil {
			return nil, fmt.Errorf("ambiguous nested block %s", key)
		}
		indexed[key] = child
	}
	return indexed, nil
}

func structuralEdits(original, replacement string, old, desired *Block) ([]sourceEdit, error) {
	if old.Type != desired.Type || !reflect.DeepEqual(old.Labels, desired.Labels) {
		return nil, fmt.Errorf("block identity mismatch")
	}
    if err := requireOpaqueFragmentsUnchanged(original,replacement,old,desired); err != nil {
        return nil,err
    }
	edits := []sourceEdit{}
	insertions := []string{}
	keys := make([]string, 0, len(old.Attrs))
	for k := range old.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		prior := old.Attrs[key]
		value, exists := desired.Attrs[key]
        if !knownEditableAttr(old,key) {
            if exists && !reflect.DeepEqual(prior,value) {
                return nil,fmt.Errorf("opaque attribute %q cannot be changed by graph patch",key)
            }
            continue
        }
		if !exists {
			r, ok := old.AttrFullRanges[key]
			if !ok {
				return nil, fmt.Errorf("cannot find full range of %s", key)
			}
			start, end := lineRemoval(original, r.Start, r.End)
			edits = append(edits, sourceEdit{start: start, end: end})
		} else if !reflect.DeepEqual(prior, value) {
			a, ao := old.AttrRanges[key]
			b, bo := desired.AttrRanges[key]
			if !ao || !bo {
				return nil, fmt.Errorf("missing attribute source span %s", key)
			}
			edits = append(edits, sourceEdit{start: a.Start, end: a.End, value: replacement[b.Start:b.End]})
		}
	}
	for _, key := range sortedAttrKeys(desired.Attrs) {
		if _, ok := old.Attrs[key]; ok {
			continue
		}
        if !knownEditableAttr(desired,key) {
            return nil,fmt.Errorf("opaque attribute %q cannot be introduced through graph patch",key)
        }
		r, exists := desired.AttrFullRanges[key]
		if !exists {
			return nil, fmt.Errorf("missing new attribute span %s", key)
		}
		start, end := fullSourceLines(replacement, r.Start, r.End)
		insertions = append(insertions, normalizeFragment(replacement[start:end], blockChildIndent(original, old)))
	}
	oldChildren, err := indexedChildren(old)
	if err != nil {
		return nil, err
	}
	desiredChildren, err := indexedChildren(desired)
	if err != nil {
		return nil, err
	}
	for _, child := range old.Children {
        if !knownNestedChild(old.Type,child.Type) {continue}
		key, _ := childIdentity(child)
		target, exists := desiredChildren[key]
		if !exists {
			start, end := lineRemoval(original, child.Start, child.End)
			edits = append(edits, sourceEdit{start: start, end: end})
		} else {
			nested, err := structuralEdits(original, replacement, child, target)
			if err != nil {
				return nil, err
			}
			edits = append(edits, nested...)
		}
	}
	for _, child := range desired.Children {
        if !knownNestedChild(desired.Type,child.Type) {continue}
		key, _ := childIdentity(child)
		if _, exists := oldChildren[key]; exists {
			continue
		}
		start, end := fullSourceLines(replacement, child.Start, child.End)
		insertions = append(insertions, normalizeFragment(replacement[start:end], blockChildIndent(original, old)))
	}
	if len(insertions) > 0 {
		pos, prefix, err := insertionPoint(original, old)
		if err != nil {
			return nil, err
		}
		edits = append(edits, sourceEdit{start: pos, end: pos, value: prefix + strings.Join(insertions, "")})
	}
	return edits, nil
}
func sortedAttrKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func lineStart(source string, pos int) int {
	if pos < 0 {
		pos = 0
	}
	if pos > len(source) {
		pos = len(source)
	}
	if n := strings.LastIndexByte(source[:pos], '\n'); n >= 0 {
		return n + 1
	}
	return 0
}
func lineEnd(source string, pos int) int {
	if pos >= len(source) {
		return len(source)
	}
	if n := strings.IndexByte(source[pos:], '\n'); n >= 0 {
		return pos + n + 1
	}
	return len(source)
}
func fullSourceLines(source string, start, end int) (int, int) {
	line := lineStart(source, start)
	if strings.TrimSpace(source[line:start]) == "" {
		start = line
	}
	if end < len(source) && strings.TrimSpace(source[end:lineEnd(source, end)]) == "" {
		end = lineEnd(source, end)
	}
	return start, end
}
func lineRemoval(source string, start, end int) (int, int) {
	return fullSourceLines(source, start, end)
}
func blockChildIndent(source string, b *Block) string {
	if len(b.AttrFullRanges) > 0 {
		first := len(source)
		for _, r := range b.AttrFullRanges {
			if r.Start < first {
				first = r.Start
			}
		}
		if first < len(source) {
			return source[lineStart(source, first):first]
		}
	}
	if len(b.Children) > 0 {
		first := b.Children[0].Start
		return source[lineStart(source, first):first]
	}
	// For an empty block indent one level relative to the block header.
	header := source[lineStart(source, b.Start):b.Start]
	return header + "  "
}
func normalizeFragment(fragment, indent string) string {
	trimmed := strings.TrimRight(fragment, "\n\r")
	if strings.TrimSpace(trimmed) == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	base := len(lines[0]) - len(strings.TrimLeft(lines[0], " \t"))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		trim := base
		if len(line) < trim {
			trim = len(line)
		}
		for j := 0; j < trim; j++ {
			if line[j] != ' ' && line[j] != '\t' {
				trim = j
				break
			}
		}
		lines[i] = indent + line[trim:]
	}
	return strings.Join(lines, "\n") + "\n"
}
func insertionPoint(source string, b *Block) (int, string, error) {
	if b.End < 1 || b.End > len(source) || source[b.End-1] != '}' {
		return 0, "", fmt.Errorf("missing block close brace at %d", b.End)
	}
	closeBrace := b.End - 1
	line := lineStart(source, closeBrace)
	if strings.TrimSpace(source[line:closeBrace]) == "" {
		return line, "", nil
	}
	return closeBrace, "\n", nil
}
func insertBeforeClose(source string, b *Block, fragment string) (string, error) {
	pos, prefix, err := insertionPoint(source, b)
	if err != nil {
		return "", err
	}
	return applyEdits(source, []sourceEdit{{start: pos, end: pos, value: prefix + fragment}})
}
func applyEdits(source string, edits []sourceEdit) (string, error) {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	right := len(source)
	for _, e := range edits {
		if e.start < 0 || e.end < e.start || e.end > len(source) {
			return "", fmt.Errorf("invalid edit range %d:%d", e.start, e.end)
		}
		if e.end > right {
			return "", fmt.Errorf("overlapping HCL edits at %d:%d", e.start, e.end)
		}
		source = source[:e.start] + e.value + source[e.end:]
		right = e.start
	}
	return source, nil
}
