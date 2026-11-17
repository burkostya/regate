package config

import (
	"strings"
	"testing"
)

func readRetryFixture() string {
	return `upstream "crm" {
  url = "https://api.example.com/mcp"
}
pipeline "reader" {
  entry = "read"
  node "read" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "lookup"
    effect = "read"
    arguments = "input"
    retry = {
      max_attempts = 3
      delay_ms = 20
      safe_read = true
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.read.value"
  }
  edge {
    from = "read"
    to = "done"
  }
}
surface "test" {
  path = "/mcp/test"
  tool "read" {
    pipeline = "reader"
    read_only = true
  }
}
`
}

func TestReadRetryRequiresExplicitBoundedOperatorAttestation(t *testing.T) {
	base := readRetryFixture()
	if _,err:=Decode(base);err!=nil {t.Fatalf("valid safe read retry rejected: %v",err)}
	cases:=[]struct{name,from,to,expected string}{
		{"unverified read effect",`effect = "read"`,`effect = "write"`,"retry requires effect=read"},
		{"unclassified effect",`effect = "read"`,`effect = "unknown"`,"retry requires effect=read"},
		{"missing assertion",`safe_read = true`,`safe_read = false`,"safe_read=true"},
		{"no assertion",`      safe_read = true
`,"","safe_read=true"},
		{"too many attempts",`max_attempts = 3`,`max_attempts = 5`,"max_attempts 2..4"},
		{"one attempt",`max_attempts = 3`,`max_attempts = 1`,"max_attempts 2..4"},
		{"excessive backoff",`delay_ms = 20`,`delay_ms = 4000`,"delay_ms 1..2000"},
		{"additional key",`      delay_ms = 20`,`      delay_ms = 20
      unreviewed = true`,"no extra fields"},
		{"bad type",`max_attempts = 3`,`max_attempts = "three"`,"max_attempts 2..4"},
	}
	for _,tc:=range cases {
		t.Run(tc.name,func(t *testing.T){
			source:=strings.Replace(base,tc.from,tc.to,1)
			if source==base {t.Fatalf("fixture replacement missing for %s",tc.name)}
			if _,err:=Decode(source);err==nil || !strings.Contains(err.Error(),tc.expected) {
				t.Fatalf("invalid retry accepted (%s): %v",tc.name,err)
			}
		})
	}
}

func TestReadRetryIsNotImplicit(t *testing.T) {
	src:=readRetryFixture()
	begin:=strings.Index(src,`    retry = {`)
	end:=strings.Index(src,`
  }
  node "done"`)
	if begin<0||end<0||begin>=end {t.Fatal("fixture changed")}
	without:=src[:begin]+src[end:]
	cfg,err:=Decode(without)
	if err!=nil {t.Fatal(err)}
	if cfg.Pipelines["reader"].Nodes["read"].Attrs["retry"]!=nil {
		t.Fatal("implicit retry was introduced")
	}
}
