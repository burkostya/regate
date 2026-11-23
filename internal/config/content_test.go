package config

import (
	"os"
	"strings"
	"testing"
)

func TestContentInterceptorConfiguration(t *testing.T) {
	example, err := os.ReadFile("../../examples/content-interceptors.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := string(example)
	cfg, err := Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	rules := cfg.ContentInterceptors("training", "prompt", "review")
	if len(rules) != 2 || rules[0].Program == nil || rules[1].Program == nil {
		t.Fatalf("prompt CEL rules missing compiled programs: %+v", rules)
	}
	for _, tt := range []struct {
		name, old, new, want string
	}{
		{"mixed selectors", `resource = "guide"`, "resource = \"guide\"\n  tool = \"guide\"", "mutually exclusive"},
		{"unknown resource", `resource = "guide"`, `resource = "unknown"`, "unknown resource"},
		{"local call stage", `stage    = "before_response"`, `stage    = "before_call"`, "requires an upstream source"},
		{"two actions", `hook     = "mask_pii"`, "hook = \"mask_pii\"\n  check = \"true\"", "requires exactly one"},
		{"invalid check type", `check   = "input.arguments.topic.matches('.*[^ ].*')"`, `check = "123"`, "expected bool"},
		{"mixed prompt sources", `text = "Review the following topic"`, "text = \"Review the following topic\"\n    source = \"upstream.review\"", "exclusive with text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			changed := strings.Replace(source, tt.old, tt.new, 1)
			if changed == source {
				t.Fatal("test fixture did not change")
			}
			if _, err := Decode(changed); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %q, got %v", tt.want, err)
			}
		})
	}
}
