package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func TestNestedResumeThroughTwoCallFrames(t *testing.T) {
	upstream,calls:=testEffectUpstream(t,false)
	defer upstream.Close()
	source:=nestedSuspensionSource(upstream.URL)
	source=strings.Replace(source, `    pipeline = "writer"
    arguments = "input"`, `    pipeline = "middle"
    arguments = "input"`,1)
	middle:=`pipeline "middle" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "child" {
    kind = "pipeline.call"
    pipeline = "writer"
    arguments = "nodes.start.value"
    allow_effects = true
    allow_suspend = true
  }
  node "done" {
    kind = "output"
    value = "nodes.child.value"
  }
  edge {
    from = "start"
    to = "child"
  }
  edge {
    from = "child"
    to = "done"
  }
}
`
	source=strings.Replace(source,`pipeline "outer" {`,middle+`pipeline "outer" {`,1)
	cfg,err:=config.Decode(source)
	if err!=nil {t.Fatal(err)}
	store:=effectTestStore(t)
	out,err:=New(store).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C600"},false)
	if err==nil || out.Status!="suspended" || calls.Load()!=1 {
		t.Fatalf("expected deep suspension: %#v %v writes=%d",out,err,calls.Load())
	}
	var cp Checkpoint
	if err:=store.GetSuspended(out.RunID,&cp);err!=nil {t.Fatal(err)}
	if cp.Child==nil || cp.Child.Child==nil ||
		cp.Child.RunID!=derivedSubrunID(out.RunID,"call","middle") ||
		cp.Child.Child.RunID!=derivedSubrunID(cp.Child.RunID,"child","writer") {
		t.Fatalf("incomplete nested call chain: %#v",cp)
	}
	done,err:=New(store).Resume(context.Background(),cfg,"rev",out.RunID,"yes")
	if err!=nil || done.Status!="success" || done.Value!="yes" || calls.Load()!=1 {
		t.Fatalf("deep resume or effect replay failed: %#v %v writes=%d",done,err,calls.Load())
	}
}

func TestSequentialNestedSuspensionsRewriteRootCheckpoint(t *testing.T) {
	upstream,calls:=testEffectUpstream(t,false)
	defer upstream.Close()
	source:=nestedSuspensionSource(upstream.URL)
	source=strings.Replace(source,`  node "done" {
    kind = "output"
    value = "nodes.wait.value"
  }`,`  node "wait_again" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  node "done" {
    kind = "output"
    value = "nodes.wait_again.value"
  }`,1)
	source=strings.Replace(source,`  edge {
    from = "wait"
    to = "done"
  }`,`  edge {
    from = "wait"
    to = "wait_again"
  }
  edge {
    from = "wait_again"
    to = "done"
  }`,1)
	cfg,err:=config.Decode(source)
	if err!=nil {t.Fatal(err)}
	store:=effectTestStore(t)
	initial,err:=New(store).Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C601"},false)
	if err==nil || initial.Status!="suspended" {t.Fatalf("first suspension failed: %#v %v",initial,err)}
	next,err:=New(store).Resume(context.Background(),cfg,"rev",initial.RunID,"first")
	if err==nil || next.Status!="suspended" || next.Checkpoint==nil ||
		next.Checkpoint.Child==nil || next.Checkpoint.Child.SuspendNode!="wait_again" {
		t.Fatalf("second suspension not persisted: %#v %v",next,err)
	}
	var cp Checkpoint
	if err:=store.GetSuspended(initial.RunID,&cp);err!=nil {t.Fatal(err)}
	if cp.Child==nil || cp.Child.SuspendNode!="wait_again" ||
		cp.Child.States["wait"]!="completed" {
		t.Fatalf("second checkpoint lost progress: %#v",cp)
	}
	finished,err:=New(store).Resume(context.Background(),cfg,"rev",initial.RunID,"second")
	if err!=nil || finished.Status!="success" || finished.Value!="second" || calls.Load()!=1 {
		t.Fatalf("second resume failed/replayed write: %#v %v writes=%d",finished,err,calls.Load())
	}
}
