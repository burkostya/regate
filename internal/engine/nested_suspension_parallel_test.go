package engine

import (
	"context"
	"database/sql"
	"strings"
	"sync"
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

func parallelSuspensionsConfig(t *testing.T,url string) *config.Config {
	t.Helper()
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
	cfg,err:=config.Decode(replaceOuterPipeline(t,nestedSuspensionSource(url),parallel))
	if err!=nil {t.Fatal(err)}
	return cfg
}

func TestMultipleSimultaneousSuspensionsDurableAndSelected(t *testing.T) {
	upstream,count:=testEffectUpstream(t,false)
	defer upstream.Close()
	cfg:=parallelSuspensionsConfig(t,upstream.URL)
	s:=effectTestStore(t)
	out,err:=New(s).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C402"},false)
	if err==nil || out.Status!="suspended" || count.Load()!=2 {
		t.Fatalf("parallel suspensions not persisted: %#v %v writes=%d",out,err,count.Load())
	}
	var cp Checkpoint
	if err:=s.GetSuspended(out.RunID,&cp);err!=nil {t.Fatal(err)}
	if len(cp.Suspensions)!=2 || cp.SuspendNode!="" || cp.Child!=nil {
		t.Fatalf("missing independent suspension records: %#v",cp)
	}
	paths:=PendingSuspensionPaths(&cp)
	if len(paths)!=2 || !equalSuspensionPath(paths[0],[]string{"first","wait"}) ||
		!equalSuspensionPath(paths[1],[]string{"second","wait"}) {
		t.Fatalf("wrong stable leaf paths: %#v",paths)
	}
	if _,err:=New(s).Resume(context.Background(),cfg,"rev",out.RunID,"ambiguous");err==nil ||
		!strings.Contains(err.Error(),"explicit suspension path") {
		t.Fatalf("ambiguous legacy resume accepted: %v",err)
	}
	if _,err:=New(s).ResumeAt(context.Background(),cfg,"rev",out.RunID,[]string{"first","wait"},42);err==nil ||
		!strings.Contains(err.Error(),"output schema") {
		t.Fatalf("invalid continuation accepted: %v",err)
	}
	first,err:=New(s).ResumeAt(context.Background(),cfg,"rev",out.RunID,[]string{"first","wait"},"one")
	if err==nil || first.Status!="suspended" || count.Load()!=2 {
		t.Fatalf("first answer did not park safely: %#v %v writes=%d",first,err,count.Load())
	}
	if _,err:=New(s).ResumeAt(context.Background(),cfg,"rev",out.RunID,[]string{"first","wait"},"duplicate");err==nil ||
		!strings.Contains(err.Error(),"already answered") {
		t.Fatalf("duplicate continuation accepted: %v",err)
	}
	if err:=s.GetSuspended(out.RunID,&cp);err!=nil {t.Fatal(err)}
	if cp.Suspensions["first"].States["wait"]!="completed" ||
		cp.Suspensions["second"].States["wait"]!="pending" {
		t.Fatalf("partial resume lost sibling checkpoint: %#v",cp)
	}
	// With only one outstanding leaf, the original Resume API stays compatible.
	finished,err:=New(s).Resume(context.Background(),cfg,"rev",out.RunID,"two")
	if err!=nil || finished.Status!="success" || count.Load()!=2 {
		t.Fatalf("parallel resume failed or replayed writes: %#v %v writes=%d",finished,err,count.Load())
	}
	if err:=s.GetSuspended(out.RunID,&cp);err!=sql.ErrNoRows {
		t.Fatalf("completed checkpoint was not removed: %v",err)
	}
}

func TestConcurrentIndependentResumePathsNeverLoseAnswers(t *testing.T) {
	upstream,calls:=testEffectUpstream(t,false)
	defer upstream.Close()
	cfg:=parallelSuspensionsConfig(t,upstream.URL)
	store:=effectTestStore(t)
	initial,err:=New(store).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C403"},false)
	if err==nil || initial.Status!="suspended" || calls.Load()!=2 {
		t.Fatalf("parallel setup failed: %#v %v writes=%d",initial,err,calls.Load())
	}
	type result struct {out Outcome;err error}
	results:=make(chan result,2)
	start:=make(chan struct{})
	var wg sync.WaitGroup
	for _,item:=range []struct{path []string;value string}{
		{[]string{"first","wait"},"one"}, {[]string{"second","wait"},"two"},
	} {
		wg.Add(1)
		go func(path []string,value string) {
			defer wg.Done()
			<-start
			out,err:=New(store).ResumeAt(context.Background(),cfg,"rev",initial.RunID,path,value)
			results<-result{out,err}
		}(item.path,item.value)
	}
	close(start)
	wg.Wait()
	close(results)
	completed,suspended:=0,0
	for res:=range results {
		switch res.out.Status {
		case "success":
			if res.err!=nil {t.Fatalf("successful resume returned error: %v",res.err)}
			completed++
		case "suspended":
			if res.err==nil {t.Fatal("suspension missing expected indication")}
			suspended++
		default:
			t.Fatalf("lost or failed concurrent continuation: %#v %v",res.out,res.err)
		}
	}
	if completed!=1 || suspended!=1 || calls.Load()!=2 {
		t.Fatalf("unexpected outcomes: complete=%d parked=%d writes=%d",completed,suspended,calls.Load())
	}
	var cp Checkpoint
	if err:=store.GetSuspended(initial.RunID,&cp);err!=sql.ErrNoRows {
		t.Fatalf("concurrently completed run retained a checkpoint: %v",err)
	}
}
