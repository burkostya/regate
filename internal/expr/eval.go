// Package expr is the CEL expression boundary for Regate. Expressions are
// compiled once at configuration activation and evaluated with a bounded CEL
// cost budget; no arbitrary Go code, reflection calls or network functions
// are exposed to users.
package expr

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
)

const MaxCost uint64 = 10000
const MaxExpressionLength = 8192

type Compiled struct {
	Source     string
	OutputType *cel.Type
	program    cel.Program
	isLiteral  bool
	literal    any
}

// Compile performs CEL parse, name resolution and type checking. The dynamic
// input/nodes values retain their runtime JSON shapes, but statically known
// type violations are rejected (e.g. 1 + true or a non-Boolean condition).
func Compile(source string, expected *cel.Type) (*Compiled, error) {
	if strings.HasPrefix(source, "literal:") {
		if expected != nil && expected != cel.DynType && expected != cel.StringType {
			return nil, fmt.Errorf("literal string cannot satisfy %s", expected)
		}
		return &Compiled{Source: source, OutputType: cel.StringType, isLiteral: true, literal: strings.TrimPrefix(source, "literal:")}, nil
	}
	if len(source) > MaxExpressionLength {
		return nil, fmt.Errorf("CEL expression exceeds %d bytes", MaxExpressionLength)
	}
	env, err := makeEnv()
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("CEL %q: %w", source, issues.Err())
	}
	got := ast.OutputType()
	if expected != nil && expected != cel.DynType && !got.IsEquivalentType(cel.DynType) && !got.IsEquivalentType(expected) {
		return nil, fmt.Errorf("CEL %q evaluates to %s, expected %s", source, got, expected)
	}
	program, err := env.Program(ast, cel.CostLimit(MaxCost))
	if err != nil {
		return nil, fmt.Errorf("CEL program %q: %w", source, err)
	}
	return &Compiled{Source: source, OutputType: got, program: program}, nil
}

func makeEnv() (*cel.Env, error) {
	strUnary := func(f func(string) string) func(ref.Val) ref.Val {
		return func(v ref.Val) ref.Val {
			s, ok := v.(types.String)
			if !ok {
				return types.NewErr("string expected")
			}
			return types.String(f(string(s)))
		}
	}
	return cel.NewEnv(
		cel.Variable("input", cel.DynType),
		cel.Variable("nodes", cel.DynType),
		// Compatibility for the pre-CEL prototype. New configurations should
		// prefer CEL receiver methods such as "alice".contains("lic").
		cel.Function("lower", cel.Overload("regate_lower_string", []*cel.Type{cel.StringType}, cel.StringType, cel.UnaryBinding(strUnary(strings.ToLower)))),
		cel.Function("upper", cel.Overload("regate_upper_string", []*cel.Type{cel.StringType}, cel.StringType, cel.UnaryBinding(strUnary(strings.ToUpper)))),
		cel.Function("contains", cel.Overload("regate_contains_string", []*cel.Type{cel.StringType, cel.StringType}, cel.BoolType, cel.BinaryBinding(func(a, b ref.Val) ref.Val {
			return types.Bool(strings.Contains(string(a.(types.String)), string(b.(types.String))))
		}))),
		cel.Function("str", cel.Overload("regate_str_dyn", []*cel.Type{cel.DynType}, cel.StringType, cel.UnaryBinding(func(v ref.Val) ref.Val { return types.String(fmt.Sprint(v.Value())) }))),
		cel.Function("redact", cel.Overload("regate_redact_dyn", []*cel.Type{cel.DynType}, cel.DynType, cel.UnaryBinding(func(v ref.Val) ref.Val {
			native, err := nativeValue(v)
			if err != nil {
				return types.NewErr("redaction input: %v", err)
			}
			return types.DefaultTypeAdapter.NativeToValue(MaskPII(native))
		}))),
	)
}
func (c *Compiled) Eval(ctx context.Context, vars map[string]any) (any, error) {
	if c.isLiteral {
		return c.literal, nil
	}
	if vars == nil {
		vars = map[string]any{}
	}
	// CEL's CostLimit is enforced while running, not merely estimated at
	// compile time. ContextEval also supports caller cancellation.
	value, _, err := c.program.ContextEval(ctx, vars)
	if err != nil {
		return nil, fmt.Errorf("CEL %q: %w", c.Source, err)
	}
	out, err := nativeValue(value)
	if err != nil {
		return nil, fmt.Errorf("CEL %q: %w", c.Source, err)
	}
	return out, nil
}
func Eval(source string, vars map[string]any) (any, error) {
	c, err := Compile(source, nil)
	if err != nil {
		return nil, err
	}
	return c.Eval(context.Background(), vars)
}

// Value is a recursively compiled expression tree for mapping/list literals.
// String leaves are CEL expressions; other primitive values are literals.
type Value struct {
	Expr    *Compiled
	Fields  map[string]*Value
	Items   []*Value
	Literal any
	Kind    string
}

func CompileValue(v any, expected *cel.Type) (*Value, error) {
	switch x := v.(type) {
	case string:
		e, err := Compile(x, expected)
		if err != nil {
			return nil, err
		}
		return &Value{Expr: e, Kind: "expr"}, nil
	case map[string]any:
		if expected != nil && expected != cel.DynType && expected != cel.MapType(cel.StringType, cel.DynType) {
			return nil, fmt.Errorf("object is incompatible with expected %s", expected)
		}
		out := &Value{Kind: "map", Fields: map[string]*Value{}}
		for key, item := range x {
			compiled, err := CompileValue(item, nil)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			out.Fields[key] = compiled
		}
		return out, nil
	case []any:
		out := &Value{Kind: "list", Items: make([]*Value, len(x))}
		for i, item := range x {
			compiled, err := CompileValue(item, nil)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			out.Items[i] = compiled
		}
		return out, nil
	default:
		if expected == cel.BoolType {
			if _, ok := v.(bool); !ok {
				return nil, fmt.Errorf("expected bool, received %T", v)
			}
		}
		return &Value{Kind: "literal", Literal: v}, nil
	}
}
func (v *Value) Eval(ctx context.Context, vars map[string]any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch v.Kind {
	case "expr":
		return v.Expr.Eval(ctx, vars)
	case "map":
		result := map[string]any{}
		for k, item := range v.Fields {
			val, err := item.Eval(ctx, vars)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			result[k] = val
		}
		return result, nil
	case "list":
		result := make([]any, len(v.Items))
		for i, item := range v.Items {
			val, err := item.Eval(ctx, vars)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			result[i] = val
		}
		return result, nil
	default:
		return v.Literal, nil
	}
}
func EvalValue(v any, vars map[string]any) (any, error) {
	p, err := CompileValue(v, nil)
	if err != nil {
		return nil, err
	}
	return p.Eval(context.Background(), vars)
}

func nativeValue(v ref.Val) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("nil CEL result")
	}
	switch x := v.(type) {
	case types.String:
		return string(x), nil
	case types.Bool:
		return bool(x), nil
	case types.Int:
		return int64(x), nil
	case types.Uint:
		return uint64(x), nil
	case types.Double:
		return float64(x), nil
	case types.Bytes:
		return []byte(x), nil
	case types.Null:
		return nil, nil
	}
	if m, ok := v.(traits.Mapper); ok {
		result := map[string]any{}
		it := m.Iterator()
		for it.HasNext() == types.True {
			key := it.Next()
			s, ok := key.(types.String)
			if !ok {
				return nil, fmt.Errorf("non-string CEL map key %T", key)
			}
			item, found := m.Find(key)
			if !found {
				return nil, fmt.Errorf("missing CEL map element")
			}
			val, err := nativeValue(item)
			if err != nil {
				return nil, err
			}
			result[string(s)] = val
		}
		return result, nil
	}
	if list, ok := v.(traits.Lister); ok {
		sz, ok := list.Size().(types.Int)
		if !ok {
			return nil, fmt.Errorf("invalid CEL list size")
		}
		if sz > 1_000_000 {
			return nil, fmt.Errorf("CEL list exceeds one million entries")
		}
		result := make([]any, int(sz))
		for i := range result {
			val, err := nativeValue(list.Get(types.Int(i)))
			if err != nil {
				return nil, err
			}
			result[i] = val
		}
		return result, nil
	}
	return v.Value(), nil
}

var authorizationValue = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Z0-9._~+/=-]+`)
var secretAssignment = regexp.MustCompile(`(?i)\b(?:access_token|refresh_token|id_token|api[_-]?key|authorization|password|client_secret|secret)[ \t]*[:=][ \t]*(?:"[^"]*"|'[^']*'|[^,; \r\n]+)`)
// Diagnostic accepts untrusted failures from upstream systems; it is deliberately
// conservative. OAuth-protected executions log only a fixed generic message.
func RedactDiagnostic(value string) string {
    if len(value)>4096 {value=value[:4096]}
    value=authorizationValue.ReplaceAllString(value,"Bearer [REDACTED]")
    value=secretAssignment.ReplaceAllString(value,"[CREDENTIAL_REDACTED]")
    return MaskPII(value).(string)
}

var email = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)

func MaskPII(v any) any {
	switch x := v.(type) {
	case string:
		return email.ReplaceAllString(x, "[EMAIL_REDACTED]")
	case map[string]any:
		y := map[string]any{}
		for k, val := range x {
			key := strings.ToLower(k)
			if strings.Contains(key, "email") || strings.Contains(key, "phone") || strings.Contains(key, "ssn") {
				y[k] = "[REDACTED]"
			} else {
				y[k] = MaskPII(val)
			}
		}
		return y
	case []any:
		y := make([]any, len(x))
		for i, item := range x {
			y[i] = MaskPII(item)
		}
		return y
	default:
		return v
	}
}
