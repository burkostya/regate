package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestToolSchemaPinsRequireLowercaseSHA256(t *testing.T) {
	const prefix = `upstream "fixture" {
  url = "http://127.0.0.1:9090/mcp"
  tool_schema_pins = %s
}
surface "fixture" {
  path = "/mcp/fixture"
  tool "call" { source = "fixture.tool.name" }
}`
	pin := strings.Repeat("a", 64)
	cfg, err := Decode(fmt.Sprintf(prefix, fmt.Sprintf(`{ "tool.name" = %q }`, pin)))
	if err != nil || cfg.Upstreams["fixture"].ToolSchemaPins["tool.name"] != pin {
		t.Fatalf("valid pin was not decoded: %v", err)
	}
	for _, bad := range []string{
		`"not an object"`, `{ tool = 42 }`, `{ tool = "abc" }`,
		fmt.Sprintf(`{ tool = %q }`, strings.ToUpper(pin)),
		fmt.Sprintf(`{ "" = %q }`, pin),
	} {
		if _, err := Decode(fmt.Sprintf(prefix, bad)); err == nil || !strings.Contains(err.Error(), "tool_schema_pins") {
			t.Errorf("invalid pins %s did not fail closed: %v", bad, err)
		}
	}
}
