package config

import (
    "strings"
    "testing"
)

func TestOAuthHCLCompilationAndSafetyRules(t *testing.T) {
    base:=example(t)
    provider:=`oauth "corp" {
      issuer = "https://issuer.test"
      audience = "https://regate.test/mcp/support"
      jwks_url = "http://127.0.0.1:12345/jwks"
    }
    `
    src:=provider+strings.Replace(base,`surface "support" {
  path = "/mcp/support"`,`surface "support" {
  path = "/mcp/support"
  oauth = "corp"
  required_scopes = ["mcp:read"]`,1)
    src=strings.Replace(src,`  tool "get_customer_direct" {`,`  tool "get_customer_direct" {
    required_scopes = ["pii:read"]
    data_classification = "restricted"
    allow_unmasked = true`,1)
    cfg,err:=Decode(src)
    if err!=nil {t.Fatalf("valid scoped HCL rejected: %v",err)}
    if cfg.Surfaces["support"].OAuth!="corp" || cfg.Surfaces["support"].Tools[1].DataClassification!="restricted" {
        t.Fatalf("lost OAuth security contracts: %#v",cfg.Surfaces["support"])
    }
    for _,tc:=range []struct{name,from,to string}{
        {"missing-provider",`oauth = "corp"`,`oauth = "missing"`},
        {"no-audience",`audience = "https://regate.test/mcp/support"`,`audience = ""`},
        {"no-jwks",`jwks_url = "http://127.0.0.1:12345/jwks"`,`jwks_url = ""`},
        {"undefined-provider",`oauth = "corp"`,`oauth = "undefined"`},
        {"duplicate-scope",`required_scopes = ["mcp:read"]`,`required_scopes = ["mcp:read", "mcp:read"]`},
        {"invalid-scope",`required_scopes = ["mcp:read"]`,`required_scopes = ["two words"]`},
        {"restricted-unscoped",`required_scopes = ["pii:read"]`,`required_scopes = []`},
        {"unsafe-unmask",`required_scopes = ["pii:read"]`,`required_scopes = ["crm:read"]`},
        {"unknown-data-label",`data_classification = "restricted"`,`data_classification = "maybe"`},
    } {
        t.Run(tc.name,func(t *testing.T) {
            altered:=strings.Replace(src,tc.from,tc.to,1)
            if altered==src {t.Fatalf("fixture substitution absent: %s",tc.from)}
            if _,err:=Decode(altered);err==nil {t.Fatal("unsafe security configuration compiled")}
        })
    }
}
func TestOAuthScopesCannotBeDeclaredWithoutAuthentication(t *testing.T) {
    src:=example(t)
    src=strings.Replace(src,`surface "support" {
  path = "/mcp/support"`,`surface "support" {
  path = "/mcp/support"
  required_scopes = ["user:read"]`,1)
    if _,err:=Decode(src);err==nil {t.Fatal("scope declarations silently ignored on anonymous surface")}
}
