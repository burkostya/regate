package expr

import (
	"cel.dev/cel-go/cel"
	"context"
	"testing"
)

func TestExpressions(t *testing.T) {
	vars := map[string]any{"input": map[string]any{"name": "Alice", "age": int64(22)}}
	for in, want := range map[string]any{"input.name": "Alice", "input.age > 18": true, "contains(lower(input.name), \"ali\")": true, "input.age + 2": int64(24)} {
		got, e := Eval(in, vars)
		if e != nil || got != want {
			t.Errorf("%s: got %v, err %v", in, got, e)
		}
	}
	if _, e := Eval("unknown(x)", vars); e == nil {
		t.Fatal("must reject unknown functions")
	}
}
func TestMask(t *testing.T) {
	in := map[string]any{"contact": "foo@example.com", "email": "foo@example.com"}
	v := MaskPII(in).(map[string]any)
	if v["email"] != "[REDACTED]" || v["contact"] != "[EMAIL_REDACTED]" {
		t.Fatal(v)
	}
}

func TestCELTypedCompileAndLimits(t *testing.T) {
	if _, err := Compile("true + 1", nil); err == nil {
		t.Fatal("CEL type-check must reject")
	}
	if _, err := Compile("'not a bool'", cel.BoolType); err == nil {
		t.Fatal("bool expectation must reject")
	}
	if _, err := Compile("unbound_var", nil); err == nil {
		t.Fatal("undeclared var must reject")
	}
	program, err := Compile("input.items.map(x, x + 1)", nil)
	if err != nil {
		t.Fatal(err)
	}
	long := make([]int64, 20000)
	for i := range long {
		long[i] = int64(i)
	}
	_, err = program.Eval(context.Background(), map[string]any{"input": map[string]any{"items": long}})
	if err == nil {
		t.Fatal("CEL cost limit must reject expensive evaluation")
	}
}
