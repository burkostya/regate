// Package expr evaluates a safe, finite expression subset used by Regate.
// It is NOT CEL and should not be presented as a CEL implementation.
package expr

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

var email = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)

func MaskPII(v any) any {
	switch x := v.(type) {
	case string:
		return email.ReplaceAllString(x, "[EMAIL_REDACTED]")
	case map[string]any:
		y := map[string]any{}
		for k, val := range x {
			if strings.Contains(strings.ToLower(k), "email") || strings.Contains(strings.ToLower(k), "phone") || strings.Contains(strings.ToLower(k), "ssn") {
				y[k] = "[REDACTED]"
			} else {
				y[k] = MaskPII(val)
			}
		}
		return y
	case []any:
		y := make([]any, len(x))
		for i, vv := range x {
			y[i] = MaskPII(vv)
		}
		return y
	default:
		return v
	}
}
func Eval(s string, vars map[string]any) (any, error) {
	if strings.HasPrefix(s, "literal:") {
		return strings.TrimPrefix(s, "literal:"), nil
	}
	tree, e := parser.ParseExpr(s)
	if e != nil {
		return nil, fmt.Errorf("expression %q: %w", s, e)
	}
	return eval(tree, vars, 0)
}
func EvalValue(x any, vars map[string]any) (any, error) {
	switch v := x.(type) {
	case string:
		return Eval(v, vars)
	case map[string]any:
		m := map[string]any{}
		for k, item := range v {
			res, e := EvalValue(item, vars)
			if e != nil {
				return nil, fmt.Errorf("%s: %w", k, e)
			}
			m[k] = res
		}
		return m, nil
	case []any:
		a := make([]any, len(v))
		for i, item := range v {
			res, e := EvalValue(item, vars)
			if e != nil {
				return nil, e
			}
			a[i] = res
		}
		return a, nil
	default:
		return x, nil
	}
}
func lookup(obj any, key string) (any, error) {
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("cannot select %q on %T", key, obj)
	}
	val, exists := m[key]
	if !exists {
		return nil, fmt.Errorf("missing key %q", key)
	}
	return val, nil
}
func eval(ex ast.Expr, vars map[string]any, depth int) (any, error) {
	if depth > 48 {
		return nil, fmt.Errorf("expression depth exceeded")
	}
	next := func(e ast.Expr) (any, error) { return eval(e, vars, depth+1) }
	switch e := ex.(type) {
	case *ast.ParenExpr:
		return next(e.X)
	case *ast.Ident:
		if e.Name == "true" {
			return true, nil
		}
		if e.Name == "false" {
			return false, nil
		}
		if e.Name == "nil" {
			return nil, nil
		}
		x, ok := vars[e.Name]
		if !ok {
			return nil, fmt.Errorf("unknown variable %s", e.Name)
		}
		return x, nil
	case *ast.SelectorExpr:
		x, er := next(e.X)
		if er != nil {
			return nil, er
		}
		return lookup(x, e.Sel.Name)
	case *ast.IndexExpr:
		x, er := next(e.X)
		if er != nil {
			return nil, er
		}
		k, er := next(e.Index)
		if er != nil {
			return nil, er
		}
		switch obj := x.(type) {
		case map[string]any:
			return lookup(obj, fmt.Sprint(k))
		case []any:
			i, ok := toFloat(k)
			if !ok || i < 0 || int(i) >= len(obj) {
				return nil, fmt.Errorf("index out of range")
			}
			return obj[int(i)], nil
		}
		return nil, fmt.Errorf("cannot index %T", x)
	case *ast.BasicLit:
		switch e.Kind {
		case token.STRING:
			return strconv.Unquote(e.Value)
		case token.INT:
			return strconv.ParseInt(e.Value, 10, 64)
		case token.FLOAT:
			return strconv.ParseFloat(e.Value, 64)
		}
	case *ast.UnaryExpr:
		v, er := next(e.X)
		if er != nil {
			return nil, er
		}
		switch e.Op {
		case token.NOT:
			b, ok := v.(bool)
			if !ok {
				return nil, fmt.Errorf("not expects bool")
			}
			return !b, nil
		case token.SUB:
			f, ok := toFloat(v)
			if !ok {
				return nil, fmt.Errorf("minus expects number")
			}
			return -f, nil
		}
	case *ast.BinaryExpr:
		a, er := next(e.X)
		if er != nil {
			return nil, er
		}
		if e.Op == token.LAND {
			b, ok := a.(bool)
			if !ok {
				return nil, fmt.Errorf("&& expects bool")
			}
			if !b {
				return false, nil
			}
		}
		if e.Op == token.LOR {
			b, ok := a.(bool)
			if !ok {
				return nil, fmt.Errorf("|| expects bool")
			}
			if b {
				return true, nil
			}
		}
		b, er := next(e.Y)
		if er != nil {
			return nil, er
		}
		switch e.Op {
		case token.EQL:
			return reflect.DeepEqual(a, b), nil
		case token.NEQ:
			return !reflect.DeepEqual(a, b), nil
		case token.LAND, token.LOR:
			vb, ok := b.(bool)
			if !ok {
				return nil, fmt.Errorf("logical op expects bool")
			}
			return vb, nil
		case token.ADD:
			if s, ok := a.(string); ok {
				return s + fmt.Sprint(b), nil
			}
		}
		af, ok1 := toFloat(a)
		bf, ok2 := toFloat(b)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("incompatible operator operands %T %T", a, b)
		}
		switch e.Op {
		case token.ADD:
			return af + bf, nil
		case token.SUB:
			return af - bf, nil
		case token.MUL:
			return af * bf, nil
		case token.QUO:
			if bf == 0 {
				return nil, fmt.Errorf("divide by zero")
			}
			return af / bf, nil
		case token.GTR:
			return af > bf, nil
		case token.GEQ:
			return af >= bf, nil
		case token.LSS:
			return af < bf, nil
		case token.LEQ:
			return af <= bf, nil
		}
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		if !ok {
			return nil, fmt.Errorf("only builtin calls allowed")
		}
		if len(e.Args) > 10 {
			return nil, fmt.Errorf("too many arguments")
		}
		args := make([]any, len(e.Args))
		for i, arg := range e.Args {
			v, er := next(arg)
			if er != nil {
				return nil, er
			}
			args[i] = v
		}
		switch id.Name {
		case "size":
			if len(args) != 1 {
				return nil, fmt.Errorf("size requires one arg")
			}
			switch v := args[0].(type) {
			case string:
				return int64(len(v)), nil
			case []any:
				return int64(len(v)), nil
			case map[string]any:
				return int64(len(v)), nil
			}
		case "lower", "upper":
			if len(args) != 1 {
				return nil, fmt.Errorf("one arg expected")
			}
			s, ok := args[0].(string)
			if !ok {
				return nil, fmt.Errorf("string expected")
			}
			if id.Name == "lower" {
				return strings.ToLower(s), nil
			}
			return strings.ToUpper(s), nil
		case "contains":
			if len(args) != 2 {
				return nil, fmt.Errorf("contains requires two args")
			}
			return strings.Contains(fmt.Sprint(args[0]), fmt.Sprint(args[1])), nil
		case "coalesce":
			for _, arg := range args {
				if arg != nil && arg != "" {
					return arg, nil
				}
			}
			return nil, nil
		case "redact":
			if len(args) != 1 {
				return nil, fmt.Errorf("redact requires one arg")
			}
			return MaskPII(args[0]), nil
		case "str":
			if len(args) != 1 {
				return nil, fmt.Errorf("str requires one arg")
			}
			return fmt.Sprint(args[0]), nil
		case "json":
			if len(args) != 1 {
				return nil, fmt.Errorf("json requires one arg")
			}
			var val any
			if err := json.Unmarshal([]byte(fmt.Sprint(args[0])), &val); err != nil {
				return nil, err
			}
			return val, nil
		}
		return nil, fmt.Errorf("unknown function %s", id.Name)
	}
	return nil, fmt.Errorf("expression operator %T unsupported", ex)
}
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, e := x.Float64()
		return f, e == nil
	}
	return 0, false
}
