package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

func mappedWriterFixture(url string) string {
	return fmt.Sprintf(`upstream "crm" {
  url = %q
  timeout_ms = 5000
}
pipeline "writer" {
  entry = "entry"
  node "entry" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["item", "index"]
      properties = {
        item = {type = "object", required = ["id"], properties = {id = {type = "string"}}}
        index = {type = "integer"}
      }
    }
  }
  node "create" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = {id = "input.item.id", index = "input.index"}
  }
  node "done" {
    kind = "output"
    value = "nodes.create.value"
    output_schema = {type = "object", required = ["created"], properties = {created = {type = "boolean"}}}
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
  entry = "batch"
  node "batch" {
    kind = "map"
    items = "input.items"
    pipeline = "writer"
    allow_effects = true
    max_items = 3
    input_schema = {type = "object", required = ["id"], properties = {id = {type = "string"}}}
    output_schema = {type = "array", items = {type = "object", required = ["created"], properties = {created = {type = "boolean"}}}}
  }
  node "done" {
    kind = "output"
    value = "nodes.batch.value"
  }
  edge {
    from = "batch"
    to = "done"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "write" {
    pipeline = "outer"
    read_only = false
  }
}
`, url)
}

func TestBoundedEffectfulMapUsesPerItemDurableEffectIdentities(t *testing.T) {
	upstream, calls := testEffectUpstream(t, false)
	defer upstream.Close()
	cfg, err := config.Decode(mappedWriterFixture(upstream.URL))
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	rt := New(store)
	args := map[string]any{"items": []any{map[string]any{"id":"A"}, map[string]any{"id":"B"}}}
	out, err := rt.Run(context.Background(), "rev", cfg, "mapped", "write", args, false)
	if err != nil || out.Status != "success" || calls.Load() != 2 {
		t.Fatalf("mapped writes not committed once per item: %#v %v calls=%d", out, err, calls.Load())
	}
	want := []any{map[string]any{"created":true}, map[string]any{"created":true}}
	if !reflect.DeepEqual(out.Value, want) {
		t.Fatalf("mapped result lost input order: got %#v, want %#v", out.Value, want)
	}
	effects := store.Effects()
	if len(effects) != 2 || effects[0].Status != "confirmed" || effects[1].Status != "confirmed" ||
		effects[0].RunID == effects[1].RunID || effects[0].RunID == out.RunID ||
		effects[1].RunID == out.RunID {
		t.Fatalf("mapped item journal scopes were not unique: %#v", effects)
	}
	_, err = rt.execute(withEffectScope(context.Background(), out.RunID),
		"rev", cfg, cfg.Pipelines["outer"], out.RunID, "mapped", "write", args, false, nil)
	if !errors.Is(err, storage.ErrEffectAlreadyAttempted) || calls.Load() != 2 {
		t.Fatalf("confirmed map item write was replayed: %v calls=%d", err, calls.Load())
	}
}

func TestBoundedEffectfulMapPrevalidatesEveryItem(t *testing.T) {
	upstream, calls := testEffectUpstream(t, false)
	defer upstream.Close()
	cfg, err := config.Decode(mappedWriterFixture(upstream.URL))
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	out, err := New(store).Run(context.Background(), "rev", cfg, "mapped", "write",
		map[string]any{"items": []any{map[string]any{"id":"A"}, map[string]any{"id":17}}}, false)
	if err == nil || !strings.Contains(err.Error(), "map item 1 failed schema validation") {
		t.Fatalf("invalid later item was not rejected before dispatch: %#v %v", out, err)
	}
	if out.Value != nil || out.OutputProduced || calls.Load() != 0 || len(store.Effects()) != 0 {
		t.Fatalf("invalid item allowed earlier mapped effect: %#v calls=%d effects=%#v",
			out, calls.Load(), store.Effects())
	}
}

func TestBoundedEffectfulMapLostResponseStopsBatchWithoutRetry(t *testing.T) {
	upstream, calls := testEffectUpstream(t, true)
	defer upstream.Close()
	cfg, err := config.Decode(mappedWriterFixture(upstream.URL))
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	rt := New(store)
	args := map[string]any{"items": []any{map[string]any{"id":"A"}, map[string]any{"id":"B"}}}
	out, err := rt.Run(context.Background(), "rev", cfg, "mapped", "write", args, false)
	if err == nil || !strings.Contains(err.Error(), "map item 0") || out.Value != nil || calls.Load() != 1 {
		t.Fatalf("ambiguous map item did not halt batch: %#v %v calls=%d", out, err, calls.Load())
	}
	effects := store.Effects()
	if len(effects) != 1 || effects[0].Status != "unknown" {
		t.Fatalf("lost map write outcome was not journaled: %#v", effects)
	}
	_, err = rt.execute(withEffectScope(context.Background(), out.RunID),
		"rev", cfg, cfg.Pipelines["outer"], out.RunID, "mapped", "write", args, false, nil)
	if !errors.Is(err, storage.ErrEffectAlreadyAttempted) || calls.Load() != 1 {
		t.Fatalf("unknown map write was retried: %v calls=%d", err, calls.Load())
	}
}

func TestMappedChildIsTransitiveTrustAndDelegationBoundary(t *testing.T) {
	cfg, err := config.Decode(mappedWriterFixture("https://api.example.com/mcp"))
	if err != nil { t.Fatal(err) }
	s := cfg.Surfaces["mapped"]
	s.OAuth = "auth-provider"
	cfg.Surfaces["mapped"] = s
	if err := enforceDataTrust(cfg, "mapped", s.Tools[0]); !errors.Is(err, ErrUntrustedDestination) {
		t.Fatalf("mapped child bypassed restricted data boundary: %v", err)
	}
	u := cfg.Upstreams["crm"]
	u.TrustLevel = "restricted"
	cfg.Upstreams["crm"] = u
	if err := enforceDataTrust(cfg, "mapped", s.Tools[0]); err != nil {
		t.Fatalf("trusted mapped child rejected: %v", err)
	}
	u.OAuthExchange = "provider"
	cfg.Upstreams["crm"] = u
	if !ToolNeedsDelegation(cfg, "mapped", s.Tools[0]) {
		t.Fatal("mapped child skipped delegated OAuth check")
	}
}

func TestMappedChildRejectsUnsupportedOrUnsafeContracts(t *testing.T) {
	src := mappedWriterFixture("https://api.example.com/mcp")
	cases := []struct{name, source, want string}{
		{"missing effect opt-in", strings.Replace(src, `    allow_effects = true`,"",1), "cannot execute inside a pure pipeline.call"},
		{"missing child", strings.Replace(src, `pipeline = "writer"`, `pipeline = "absent"`,1), "unknown mapped pipeline absent"},
		{"conflicting CEL mapping", strings.Replace(src, `pipeline = "writer"`, `pipeline = "writer"\n    mapping = "upper(input)"`,1), "exactly one of mapping and pipeline"},
		{"effectful map missing items schema", strings.Replace(src,
			`output_schema = {type = "array", items = {type = "object", required = ["created"], properties = {created = {type = "boolean"}}}}`,
			`output_schema = {type = "array"}`,1), "effectful map requires array output_schema with items"},
		{"child output type incompatible", strings.Replace(src,
			`output_schema = {type = "array", items = {type = "object", required = ["created"], properties = {created = {type = "boolean"}}}}`,
			`output_schema = {type = "array", items = {type = "string"}}`,1), "output item contract"},
		{"child item type incompatible", strings.Replace(src,
			`input_schema = {type = "object", required = ["id"], properties = {id = {type = "string"}}}`,
			`input_schema = {type = "integer"}`,1), "item contract"},
		{"nested suspend rejected", strings.Replace(src, `kind = "mcp.call"`, `kind = "suspend"`,1), "suspend requires durable nested checkpoint support"},
		{"unapproved parent of effectful map", strings.Replace(src,
			`pipeline "outer" {`, `pipeline "mid" {
  entry = "batch"
  node "batch" {
    kind = "map"
    items = "input.items"
    pipeline = "writer"
    allow_effects = true
    max_items = 3
    output_schema = {type = "array", items = {type = "object"}}
  }
  node "done" {
    kind = "output"
    value = "nodes.batch.value"
  }
  edge {
    from = "batch"
    to = "done"
  }
}
pipeline "outer" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "mid"
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
pipeline "unused" {`,1), "pure parent cannot delegate effects through map"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := config.Decode(tc.source); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q; got %v", tc.want, err)
			}
		})
	}
}

func pureMappedChildFixture() string {
	return `pipeline "upper_child" {
  entry = "start"
  node "start" {
    kind = "input"
    input_schema = {type = "object", properties = {item = {type = "string"}, index = {type = "integer"}}, required = ["item", "index"]}
  }
  node "done" {
    kind = "output"
    value = "upper(input.item)"
    output_schema = {type = "string"}
  }
  edge {
    from = "start"
    to = "done"
  }
}
pipeline "outer" {
  entry = "batch"
  node "batch" {
    kind = "map"
    items = "input.items"
    pipeline = "upper_child"
    max_items = 3
    input_schema = {type = "string"}
    output_schema = {type = "array", items = {type = "string"}}
  }
  node "done" {
    kind = "output"
    value = "nodes.batch.value"
  }
  edge {
    from = "batch"
    to = "done"
  }
}
surface "mapped" {
  path = "/mcp/mapped"
  tool "upper" {
    pipeline = "outer"
    read_only = true
  }
}
`
}

func TestPureMappedChildRetainsOrderAndBounds(t *testing.T) {
	cfg, err := config.Decode(pureMappedChildFixture())
	if err != nil { t.Fatal(err) }
	for _, tc := range []struct {input, want []any}{
		{[]any{"alice", "Bob"}, []any{"ALICE", "BOB"}},
		{[]any{}, []any{}},
	} {
		out, err := New(nil).Run(context.Background(), "rev", cfg, "mapped", "upper",
			map[string]any{"items": tc.input}, true)
		if err != nil || !reflect.DeepEqual(out.Value, tc.want) {
			t.Fatalf("pure mapped child result: %#v %v, want %#v", out, err, tc.want)
		}
	}
}
