package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func replaceOuterPipeline(t *testing.T, source, replacement string) string {
	t.Helper()
	begin:=strings.Index(source,`pipeline "outer" {`)
	end:=strings.Index(source,`surface "nested" {`)
	if begin<0 || end<=begin {t.Fatal("nested fixture changed")}
	return source[:begin]+replacement+source[end:]
}

func TestNestedSuspensionSettlesParallelExternalWrite(t *testing.T) {
	upstream,count:=testEffectUpstream(t,false)
	defer upstream.Close()
	parallel:=`pipeline "outer" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    arguments = "nodes.entry.value"
    allow_effects = true
    allow_suspend = true
  }
  node "sibling" {
    kind = "mcp.call"
    upstream = "crm"
    tool = "create"
    effect = "write"
    arguments = "nodes.entry.value"
  }
  node "barrier" {
    kind = "join"
    mode = "all"
    value = "nodes.call.value"
  }
  node "done" {
    kind = "output"
    value = "nodes.barrier.value"
  }
  edge {
    from = "entry"
    to = "call"
  }
  edge {
    from = "entry"
    to = "sibling"
  }
  edge {
    from = "call"
    to = "barrier"
  }
  edge {
    from = "sibling"
    to = "barrier"
  }
  edge {
    from = "barrier"
    to = "done"
  }
}
`
	cfg,err:=config.Decode(replaceOuterPipeline(t,nestedSuspensionSource(upstream.URL),parallel))
	if err!=nil {t.Fatal(err)}
	s:=effectTestStore(t)
	out,err:=New(s).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C401"},false)
	if err==nil || out.Status!="suspended" || count.Load()!=2 {
		t.Fatalf("sibling wave failed to settle: %#v %v writes=%d",out,err,count.Load())
	}
	var cp Checkpoint
	if err:=s.GetSuspended(out.RunID,&cp);err!=nil {t.Fatal(err)}
	if cp.States["sibling"]!="completed" || cp.States["call"]!="pending" {
		t.Fatalf("parallel sibling was not checkpointed: %#v",cp.States)
	}
	resumed,err:=New(s).Resume(context.Background(),cfg,"rev",out.RunID,"ready")
	if err!=nil || resumed.Value!="ready" || count.Load()!=2 {
		t.Fatalf("resume replayed parallel write: %#v %v writes=%d",resumed,err,count.Load())
	}
}

func TestMultipleSimultaneousSuspensionsFailClosed(t *testing.T) {
	upstream,count:=testEffectUpstream(t,false)
	defer upstream.Close()
	parallel:=`pipeline "outer" {
  entry = "entry"
  node "entry" {
    kind = "input"
  }
  node "first" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    allow_suspend = true
    arguments = "nodes.entry.value"
  }
  node "second" {
    kind = "pipeline.call"
    pipeline = "writer"
    allow_effects = true
    allow_suspend = true
    arguments = "nodes.entry.value"
  }
  node "barrier" {
    kind = "join"
    mode = "all"
  }
  node "done" {
    kind = "output"
    value = "nodes.entry.value"
  }
  edge {
    from = "entry"
    to = "first"
  }
  edge {
    from = "entry"
    to = "second"
  }
  edge {
    from = "first"
    to = "barrier"
  }
  edge {
    from = "second"
    to = "barrier"
  }
  edge {
    from = "barrier"
    to = "done"
  }
}
`
	cfg,err:=config.Decode(replaceOuterPipeline(t,nestedSuspensionSource(upstream.URL),parallel))
	if err!=nil {t.Fatal(err)}
	s:=effectTestStore(t)
	out,err:=New(s).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C402"},false)
	if err==nil || !strings.Contains(err.Error(),"simultaneous suspension") ||
		out.Status=="suspended" || count.Load()!=2 {
		t.Fatalf("ambiguous checkpoint accepted: %#v %v writes=%d",out,err,count.Load())
	}
	var cp Checkpoint
	if err:=s.GetSuspended(out.RunID,&cp);err==nil {
		t.Fatal("ambiguous checkpoint was published")
	}
}
