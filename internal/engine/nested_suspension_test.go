package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/burkostya/regate/internal/config"
)

func nestedSuspensionSource(url string) string {
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
  node "wait" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  node "done" {
    kind = "output"
    value = "nodes.wait.value"
  }
  edge {
    from = "entry"
    to = "create"
  }
  edge {
    from = "create"
    to = "wait"
  }
  edge {
    from = "wait"
    to = "done"
  }
}
pipeline "outer" {
  entry = "call"
  node "call" {
    kind = "pipeline.call"
    pipeline = "writer"
    arguments = "input"
    allow_effects = true
    allow_suspend = true
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
  }
}
`,url)
}

func TestNestedCheckpointResumeAndValueContract(t *testing.T) {
	upstream,count:=testEffectUpstream(t,false)
	defer upstream.Close()
	cfg,err:=config.Decode(nestedSuspensionSource(upstream.URL))
	if err!=nil {t.Fatal(err)}
	store:=effectTestStore(t)
	r:=New(store)
	out,err:=r.Run(context.Background(),"rev",cfg,"nested","write",
		map[string]any{"id":"C400"},false)
	if err==nil || out.Status!="suspended" || count.Load()!=1 {
		t.Fatalf("unexpected first execution: %#v %v writes=%d",out,err,count.Load())
	}
	var cp Checkpoint
	if err:=store.GetSuspended(out.RunID,&cp);err!=nil {t.Fatal(err)}
	if cp.SuspendNode!="call" || cp.Child==nil ||
		cp.Child.RunID!=derivedSubrunID(out.RunID,"call","writer") ||
		cp.Child.SuspendNode!="wait" || cp.Child.States["create"]!="completed" {
		t.Fatalf("nested checkpoint was not preserved: %#v",cp)
	}
	if _,err:=r.Resume(context.Background(),cfg,"rev",out.RunID,42);
		err==nil || !strings.Contains(err.Error(),"output schema") {
		t.Fatalf("invalid leaf continuation accepted: %v",err)
	}
	resumed,err:=New(store).Resume(context.Background(),cfg,"rev",out.RunID,"ready")
	if err!=nil || resumed.Status!="success" || resumed.Value!="ready" {
		t.Fatalf("nested continuation failed: %#v %v",resumed,err)
	}
	if count.Load()!=1 {t.Fatalf("external write replayed: %d",count.Load())}
	if err:=store.GetSuspended(out.RunID,&cp);err!=sql.ErrNoRows {
		t.Fatalf("root checkpoint was not deleted: %v",err)
	}
}
