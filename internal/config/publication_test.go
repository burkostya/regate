package config

import (
	"strings"
	"testing"
)

func TestPublishedUpstreamConfiguration(t *testing.T) {
	const upstream = "upstream \"backend\" {\n url = \"http://127.0.0.1:9091/mcp\"\n}\n"
	const surface = "surface \"edge\" {\n upstream = \"backend\"\n auth = \"anonymous\"\n}\n"
	cfg, err := Decode(upstream + surface)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Surfaces["edge"]; got.Path != "/mcp/edge" || got.Upstream != "backend" || got.Auth != "anonymous" {
		t.Fatalf("unexpected publication: %+v", got)
	}
	cfg, err = Decode(upstream)
	if err != nil || len(cfg.Surfaces) != 0 {
		t.Fatalf("declaring an upstream also published it: %v", err)
	}
	for _, tc := range []struct {
		name, source, want string
	}{
		{"missing auth", upstream + strings.Replace(surface, " auth = \"anonymous\"\n", "", 1), "requires auth"},
		{"missing upstream", strings.Replace(upstream+surface, "upstream = \"backend\"", "upstream = \"missing\"", 1), "undefined upstream"},
		{"reserved", upstream + strings.Replace(surface, " auth", " path = \"/mcp/admin\"\n auth", 1), "reserved path"},
		{"reserved prefix", upstream + strings.Replace(surface, " auth", " path = \"/api/public\"\n auth", 1), "reserved path"},
		{"noncanonical", upstream + strings.Replace(surface, " auth", " path = \"/mcp/x/../edge\"\n auth", 1), "canonical path"},
		{"duplicate", upstream + surface + "surface \"other\" { path = \"/mcp/edge\" }", "duplicate path"},
		{"mixed", upstream + strings.Replace(surface, " auth = \"anonymous\"\n", " auth = \"anonymous\"\n prompt \"local\" { text = \"test\" }\n", 1), "cannot include local"},
		{"anonymous with gateway credential", strings.Replace(upstream, "\n url", "\n auth_env = \"BACKEND_TOKEN\"\n url", 1) + surface, "without configured credentials"},
		{"bearer without secrets", upstream + strings.Replace(surface, "anonymous", "bearer", 1), "requires distinct public bearer_token_env"},
		{"bearer with shared variable", strings.Replace(upstream, "\n url", "\n auth_env = \"BACKEND_TOKEN\"\n url", 1) + strings.Replace(surface, "auth = \"anonymous\"", "auth = \"bearer\"\n bearer_token_env = \"BACKEND_TOKEN\"", 1), "requires distinct public bearer_token_env"},
		{"old surface without path", upstream + "surface \"existing\" {}", "empty path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}
