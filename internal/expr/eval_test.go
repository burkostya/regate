package expr

import "testing"

func TestExpressions(t *testing.T) {
	vars := map[string]any{"input": map[string]any{"name": "Alice", "age": int64(22)}}
	for in, want := range map[string]any{"input.name": "Alice", "input.age > 18": true, "contains(lower(input.name), \"ali\")": true, "input.age + 2": float64(24)} {
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
