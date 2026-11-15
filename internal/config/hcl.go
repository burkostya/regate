// Package config provides a deliberately bounded, dependency-free HCL-like
// parser. Unsupported HCL2 expressions are rejected, not silently ignored.
// Replace this parser with hashicorp/hcl/v2 before claiming full HCL2 support.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Block struct {
	Type     string         `json:"type"`
	Labels   []string       `json:"labels,omitempty"`
	Attrs    map[string]any `json:"attrs,omitempty"`
	Children []*Block       `json:"children,omitempty"`
	Start    int            `json:"start"`
	End      int            `json:"end"`
}

type token struct {
	k, v string
	pos  int
}
type lexer struct {
	s string
	i int
}

func (l *lexer) next() (token, error) {
	for l.i < len(l.s) {
		p := l.i
		r, n := utf8.DecodeRuneInString(l.s[l.i:])
		if unicode.IsSpace(r) {
			l.i += n
			continue
		}
		if r == '#' || strings.HasPrefix(l.s[l.i:], "//") {
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
			continue
		}
		if strings.HasPrefix(l.s[l.i:], "/*") {
			j := strings.Index(l.s[l.i+2:], "*/")
			if j < 0 {
				return token{}, fmt.Errorf("unclosed comment at %d", p)
			}
			l.i += j + 4
			continue
		}
		if r == '"' {
			l.i += n
			escaped := false
			for l.i < len(l.s) {
				c := l.s[l.i]
				l.i++
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == '"' {
					var value string
					if err := json.Unmarshal([]byte(l.s[p:l.i]), &value); err != nil {
						return token{}, err
					}
					return token{"str", value, p}, nil
				}
			}
			return token{}, fmt.Errorf("unclosed string at %d", p)
		}
		if strings.ContainsRune("{}[]=,:()", r) {
			l.i += n
			return token{string(r), string(r), p}, nil
		}
		if unicode.IsLetter(r) || r == '_' || r == '-' || unicode.IsDigit(r) {
			l.i += n
			for l.i < len(l.s) {
				rr, nn := utf8.DecodeRuneInString(l.s[l.i:])
				if unicode.IsLetter(rr) || unicode.IsDigit(rr) || rr == '_' || rr == '-' || rr == '.' {
					l.i += nn
				} else {
					break
				}
			}
			v := l.s[p:l.i]
			return token{"word", v, p}, nil
		}
		return token{}, fmt.Errorf("unexpected character %q at offset %d", r, p)
	}
	return token{"eof", "", len(l.s)}, nil
}

type parser struct {
	tokens []token
	at     int
	source string
}

func (p *parser) peek() token { return p.tokens[p.at] }
func (p *parser) pop() token {
	t := p.peek()
	if p.at < len(p.tokens)-1 {
		p.at++
	}
	return t
}
func (p *parser) want(k string) (token, error) {
	t := p.pop()
	if t.k != k {
		return t, fmt.Errorf("expected %s at %d, found %q", k, t.pos, t.v)
	}
	return t, nil
}
func (p *parser) value() (any, error) {
	t := p.pop()
	switch t.k {
	case "str":
		return t.v, nil
	case "word":
		switch t.v {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		if i, e := strconv.ParseInt(t.v, 10, 64); e == nil {
			return i, nil
		}
		if f, e := strconv.ParseFloat(t.v, 64); e == nil {
			return f, nil
		}
		return nil, fmt.Errorf("unsupported HCL expression %q at %d; use quoted literal or supported map/list", t.v, t.pos)
	case "[":
		a := []any{}
		for p.peek().k != "]" {
			if p.peek().k == "eof" {
				return nil, fmt.Errorf("unterminated list at %d", t.pos)
			}
			v, e := p.value()
			if e != nil {
				return nil, e
			}
			a = append(a, v)
			if p.peek().k == "," {
				p.pop()
			}
		}
		p.pop()
		return a, nil
	case "{":
		m := map[string]any{}
		for p.peek().k != "}" {
			if p.peek().k == "eof" {
				return nil, fmt.Errorf("unterminated object at %d", t.pos)
			}
			key := p.pop()
			if key.k != "word" && key.k != "str" {
				return nil, fmt.Errorf("invalid object key at %d", key.pos)
			}
			if p.peek().k != "=" && p.peek().k != ":" {
				return nil, fmt.Errorf("object expects = at %d", p.peek().pos)
			}
			p.pop()
			val, e := p.value()
			if e != nil {
				return nil, e
			}
			if _, ok := m[key.v]; ok {
				return nil, fmt.Errorf("duplicate key %s", key.v)
			}
			m[key.v] = val
			if p.peek().k == "," {
				p.pop()
			}
		}
		p.pop()
		return m, nil
	default:
		return nil, fmt.Errorf("invalid value %q at %d", t.v, t.pos)
	}
}
func (p *parser) body(kind string, labels []string, start int) (*Block, error) {
	b := &Block{Type: kind, Labels: labels, Attrs: map[string]any{}, Start: start}
	for p.peek().k != "}" {
		if p.peek().k == "eof" {
			return nil, fmt.Errorf("unterminated block %s", kind)
		}
		name := p.pop()
		if name.k != "word" {
			return nil, fmt.Errorf("expected identifier at %d, found %s", name.pos, name.v)
		}
		if p.peek().k == "=" {
			p.pop()
			v, e := p.value()
			if e != nil {
				return nil, e
			}
			if _, has := b.Attrs[name.v]; has {
				return nil, fmt.Errorf("duplicate attribute %s in %s", name.v, kind)
			}
			b.Attrs[name.v] = v
		} else {
			lbl := []string{}
			for p.peek().k == "str" || p.peek().k == "word" {
				lbl = append(lbl, p.pop().v)
			}
			if _, e := p.want("{"); e != nil {
				return nil, e
			}
			child, e := p.body(name.v, lbl, name.pos)
			if e != nil {
				return nil, e
			}
			b.Children = append(b.Children, child)
		}
	}
	close := p.pop()
	b.End = close.pos + 1
	return b, nil
}
func Parse(src string) ([]*Block, error) {
	l := lexer{s: src}
	ts := []token{}
	for {
		t, e := l.next()
		if e != nil {
			return nil, e
		}
		ts = append(ts, t)
		if t.k == "eof" {
			break
		}
	}
	p := parser{tokens: ts, source: src}
	ret := []*Block{}
	for p.peek().k != "eof" {
		t := p.pop()
		if t.k != "word" {
			return nil, fmt.Errorf("expected HCL block at %d", t.pos)
		}
		labels := []string{}
		for p.peek().k == "str" || p.peek().k == "word" {
			labels = append(labels, p.pop().v)
		}
		if _, e := p.want("{"); e != nil {
			return nil, e
		}
		b, e := p.body(t.v, labels, t.pos)
		if e != nil {
			return nil, e
		}
		ret = append(ret, b)
	}
	return ret, nil
}
func ParseFile(path string) ([]*Block, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return Parse(string(b))
}
func (b *Block) Label() (string, error) {
	if len(b.Labels) != 1 {
		return "", fmt.Errorf("%s needs exactly one label", b.Type)
	}
	return b.Labels[0], nil
}
func (b *Block) String(k string) string { v, _ := b.Attrs[k].(string); return v }
func (b *Block) Int(k string, def int) int {
	if v, ok := b.Attrs[k].(int64); ok {
		return int(v)
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
	res := []*Block{}
	for _, x := range b.Children {
		if x.Type == kind {
			res = append(res, x)
		}
	}
	return res
}
func (b *Block) Strings(k string) []string {
	a, _ := b.Attrs[k].([]any)
	r := []string{}
	for _, x := range a {
		if s, ok := x.(string); ok {
			r = append(r, s)
		}
	}
	return r
}
