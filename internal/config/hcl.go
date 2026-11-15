// Package config parses Regate's executable configuration using the official
// HCL2 syntax tree. Source positions are byte offsets into the original file:
// graph and MCP edits never round-trip unrelated blocks through a formatter.
package config

import (
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

type Block struct {
	Type       string                 `json:"type"`
	Labels     []string               `json:"labels,omitempty"`
	Attrs      map[string]any         `json:"attrs,omitempty"`
	Children   []*Block               `json:"children,omitempty"`
	Start      int                    `json:"start"`
	End        int                    `json:"end"`
	Line       int                    `json:"line"`
	AttrRanges map[string]SourceRange `json:"attr_ranges,omitempty"`
}

type SourceRange struct {
	Start  int `json:"start"`
	End    int `json:"end"`
	Line   int `json:"line"`
	Column int `json:"column"`
}

func Parse(source string) ([]*Block, error) {
	file, diags := hclsyntax.ParseConfig([]byte(source), "regate.hcl", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("HCL2: %s", diags.Error())
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected HCL2 body type %T", file.Body)
	}
	blocks := make([]*Block, 0, len(body.Blocks))
	for _, node := range body.Blocks {
		block, err := readBlock(node)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	if len(body.Attributes) > 0 {
		return nil, fmt.Errorf("HCL2: top-level attributes are not supported; use named blocks")
	}
	return blocks, nil
}

func readBlock(node *hclsyntax.Block) (*Block, error) {
	r := node.Range()
	b := &Block{Type: node.Type, Labels: append([]string(nil), node.Labels...), Attrs: map[string]any{}, Start: r.Start.Byte, End: r.End.Byte, Line: r.Start.Line, AttrRanges: map[string]SourceRange{}}
	names := make([]string, 0, len(node.Body.Attributes))
	for name := range node.Body.Attributes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := node.Body.Attributes[name]
		val, diags := a.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s[%s] attribute %s at %s: %s", b.Type, joinLabels(b.Labels), name, a.Expr.Range().String(), diags.Error())
		}
		result, err := ctyToGo(val)
		if err != nil {
			return nil, fmt.Errorf("%s[%s] attribute %s: %w", b.Type, joinLabels(b.Labels), name, err)
		}
		b.Attrs[name] = result
		ar := a.Expr.Range()
		b.AttrRanges[name] = SourceRange{Start: ar.Start.Byte, End: ar.End.Byte, Line: ar.Start.Line, Column: ar.Start.Column}
	}
	for _, child := range node.Body.Blocks {
		parsed, err := readBlock(child)
		if err != nil {
			return nil, err
		}
		b.Children = append(b.Children, parsed)
	}
	return b, nil
}
func joinLabels(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	return fmt.Sprint(labels)
}

// ctyToGo is intentionally lossless for JSON-compatible HCL values: integer
// values stay int64, nonintegral numbers become float64, and dynamic/unknown
// values are refused before publication. Values with larger than int64 integer
// precision are rejected rather than rounded into a different configuration.
func ctyToGo(v cty.Value) (any, error) {
	if !v.IsKnown() {
		return nil, fmt.Errorf("unknown HCL value is not executable")
	}
	if v.IsNull() {
		return nil, nil
	}
	switch {
	case v.Type() == cty.String:
		return v.AsString(), nil
	case v.Type() == cty.Bool:
		return v.True(), nil
	case v.Type() == cty.Number:
		f := v.AsBigFloat()
		if i, accuracy := f.Int64(); accuracy == big.Exact {
			return i, nil
		}
		float, _ := f.Float64()
		if !math.IsInf(float, 0) && !math.IsNaN(float) {
			return float, nil
		}
		return nil, fmt.Errorf("number is outside float64 range")
	case v.Type().IsObjectType(), v.Type().IsMapType():
		m := map[string]any{}
		iter := v.ElementIterator()
		for iter.Next() {
			k, val := iter.Element()
			item, err := ctyToGo(val)
			if err != nil {
				return nil, err
			}
			m[k.AsString()] = item
		}
		return m, nil
	case v.Type().IsTupleType(), v.Type().IsListType(), v.Type().IsSetType():
		a := make([]any, 0, v.LengthInt())
		it := v.ElementIterator()
		for it.Next() {
			_, val := it.Element()
			item, err := ctyToGo(val)
			if err != nil {
				return nil, err
			}
			a = append(a, item)
		}
		return a, nil
	default:
		return nil, fmt.Errorf("unsupported HCL value type %s", v.Type().FriendlyName())
	}
}
func ParseFile(path string) ([]*Block, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(string(src))
}
func (b *Block) Label() (string, error) {
	if len(b.Labels) != 1 {
		return "", fmt.Errorf("%s needs exactly one label", b.Type)
	}
	return b.Labels[0], nil
}
func (b *Block) String(k string) string { s, _ := b.Attrs[k].(string); return s }
func (b *Block) Int(k string, def int) int {
	if i, ok := b.Attrs[k].(int64); ok {
		return int(i)
	}
	return def
}
func (b *Block) Bool(k string, def bool) bool {
	if v, ok := b.Attrs[k].(bool); ok {
		return v
	}
	return def
}
func (b *Block) Child(kind string) []*Block {
	a := []*Block{}
	for _, b2 := range b.Children {
		if b2.Type == kind {
			a = append(a, b2)
		}
	}
	return a
}
