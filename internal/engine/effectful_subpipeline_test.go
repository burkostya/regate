package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

func nestedEffectSource(url string) string {
	return fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "writer" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.create.value"
  }
  edge {
    from = "entry"
    to = "create"
  }
  edge {
    from = "create"
    to = "done"
  }
}
pipeline "outer" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    arguments = "input"
  }
  node "done" {
    kind = "output"
    value = "nodes.call.value"
  }
  edge {
    from = "call"
    to = "done"
  }
}
surface "nested" {
  path = "/mcp/nested"
  tool "write" {
    pipeline = "outer"
    read_only = false
  }
}
`,url)
}

func TestNestedEffectUsesDurableSubrunWithoutReplayingWrite(t *testing.T) {
	upstream, calls := testEffectUpstream(t, false)
	defer upstream.Close()
	cfg, err := config.Decode(nestedEffectSource(upstream.URL))
	if err != nil { t.Fatal(err) }
	s := effectTestStore(t)
	runtime := New(s)
	args := map[string]any{"id":"C100"}
	out, err := runtime.Run(context.Background(), "rev", cfg, "nested", "write", args, false)
	if err != nil { t.Fatal(err) }
	if out.Status != "success" || out.RunID == "" || calls.Load() != 1 {
		t.Fatalf("child did not complete exactly one write: %#v, calls=%d",out,calls.Load())
	}
	value, ok := out.Value.(map[string]any)
	if !ok || value["created"] != true {t.Fatalf("nested write result missing: %#v",out.Value)}
	records := s.Effects()
	if len(records) != 1 || records[0].Status != "confirmed" || records[0].RunID == out.RunID {
		t.Fatalf("nested effect not scoped durably: %#v",records)
	}
	_, err = runtime.execute(withEffectScope(context.Background(),out.RunID),"rev",cfg,
		cfg.Pipelines["outer"],out.RunID,"nested","write",args,false,nil)
	if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
		t.Fatalf("nested confirmed effect replay not blocked: %v",err)
	}
	if calls.Load() != 1 {t.Fatalf("replayed external child write %d times",calls.Load())}
}

func TestNestedUnknownEffectCannotReplayAfterLostResponse(t *testing.T) {
	upstream, calls := testEffectUpstream(t, true)
	defer upstream.Close()
	cfg, err := config.Decode(nestedEffectSource(upstream.URL))
	if err != nil {t.Fatal(err)}
	s := effectTestStore(t)
	runtime := New(s)
	args := map[string]any{"id":"C200"}
	out, err := runtime.Run(context.Background(),"rev",cfg,"nested","write",args,false)
	if err == nil { t.Fatal("ambiguous nested external effect returned success") }
	records := s.Effects()
	if len(records)!=1 || records[0].Status!="unknown" || calls.Load()!=1 {
		t.Fatalf("nested unknown outcome lost: %#v calls=%d err=%v",records,calls.Load(),err)
	}
	_,err=runtime.execute(withEffectScope(context.Background(),out.RunID),"rev",cfg,
		cfg.Pipelines["outer"],out.RunID,"nested","write",args,false,nil)
	if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
		t.Fatalf("unknown nested effect was retried: %v",err)
	}
	if calls.Load()!=1 {t.Fatal("ambiguous nested write repeated")}
}

func TestNestedTrustBoundaryAndDelegationAreTransitive(t *testing.T) {
	cfg, err := config.Decode(nestedEffectSource("https://api.example.com/mcp"))
	if err != nil { t.Fatal(err) }
	surface := cfg.Surfaces["nested"]
	surface.OAuth = "auth-provider" // authenticated input is conservatively Restricted
	cfg.Surfaces["nested"] = surface
	tool := surface.Tools[0]

	if err := enforceDataTrust(cfg,"nested",tool); !errors.Is(err,ErrUntrustedDestination) {
		t.Fatalf("nested untrusted upstream bypassed data boundary: %v",err)
	}
	upstream := cfg.Upstreams["crm"]
	upstream.TrustLevel = "restricted"
	cfg.Upstreams["crm"] = upstream
	if err := enforceDataTrust(cfg,"nested",tool); err != nil {
		t.Fatalf("trusted nested upstream rejected: %v",err)
	}
	if ToolNeedsDelegation(cfg,"nested",tool) {
		t.Fatal("undelegated nested upstream incorrectly marked")
	}
	upstream.OAuthExchange = "provider"
	cfg.Upstreams["crm"] = upstream
	if !ToolNeedsDelegation(cfg,"nested",tool) {
		t.Fatal("nested OAuth exchange escaped async delegation checks")
	}
	// A direct runtime graph mutation cannot safely hide a missing callee.
	outer := cfg.Pipelines["outer"]
	call := outer.Nodes["call"]
	call.Attrs["pipeline"] = "missing"
	outer.Nodes["call"] = call
	cfg.Pipelines["outer"] = outer
	if err := enforceDataTrust(cfg,"nested",tool); err == nil {
		t.Fatal("unresolvable call escaped trust traversal")
	}
	if !ToolNeedsDelegation(cfg,"nested",tool) {
		t.Fatal("unresolvable call accepted without execution grant")
	}
}

func TestNestedHTTPHookSharesScopedEffectJournal(t *testing.T) {
	var calls atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type","application/json")
		fmt.Fprint(w,`{"value":{"created":true}}`)
	}))
	defer endpoint.Close()
	// Replace child MCP step with a declared HTTP hook. Both must use the
	// same nested run identity; the write journal must block repeat dispatch.
	source := nestedEffectSource("https://api.example.com/mcp")
	source = strings.Replace(source,`pipeline "writer" {`,fmt.Sprintf(`hook "writer_hook" {
  transport = "http"
  contract = "transform"
  url = %q
}
pipeline "writer" {`,endpoint.URL),1)
	source = strings.Replace(source,`  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }`,`  node "create" {
    kind = "hook.call"
    hook = "writer_hook"
    value = "nodes.entry.value"
  }`,1)
	cfg, err := config.Decode(source)
	if err != nil {t.Fatal(err)}
	s := effectTestStore(t)
	runtime := New(s)
	args := map[string]any{"id":"C300"}
	out, err := runtime.Run(context.Background(),"rev",cfg,"nested","write",args,false)
	if err != nil {t.Fatal(err)}
	if calls.Load()!=1 || len(s.Effects())!=1 || s.Effects()[0].RunID==out.RunID {
		t.Fatalf("hook did not use scoped nested effect: %#v calls=%d",s.Effects(),calls.Load())
	}
	_,err=runtime.execute(withEffectScope(context.Background(),out.RunID),"rev",cfg,
		cfg.Pipelines["outer"],out.RunID,"nested","write",args,false,nil)
	if !errors.Is(err,storage.ErrEffectAlreadyAttempted) {
		t.Fatalf("nested HTTP hook replay accepted: %v",err)
	}
	if calls.Load()!=1 {t.Fatal("nested HTTP hook was dispatched twice")}
}
