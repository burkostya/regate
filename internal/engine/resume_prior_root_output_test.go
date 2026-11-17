package engine

import (
    "context"
    "database/sql"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
)

// A root output can finish in parallel with an unrelated suspension. That
// output must be released only once all branches have settled, even when the
// terminal itself is not re-executed during Resume.
func persistedRootOutputFixture() string {
    return `pipeline "root" {
  entry = "start"
  node "start" {
    kind = "input"
  }
  node "early" {
    kind = "output"
    value = "literal:alice@example.com"
    output_schema = {type = "string"}
  }
  node "pause" {
    kind = "suspend"
    output_schema = {type = "string"}
  }
  edge {
    from = "start"
    to = "early"
  }
  edge {
    from = "start"
    to = "pause"
  }
}
surface "root" {
  path = "/mcp/root"
  tool "read" {
    pipeline = "root"
    read_only = true
    output_schema = {type = "string"}
  }
}
`
}

func TestResumeReconstructsPriorRootTerminalAndAppliesResponsePolicy(t *testing.T) {
    cfg,err:=config.Decode(withBuiltinResponseMask(persistedRootOutputFixture()))
    if err!=nil {t.Fatal(err)}
    store:=effectTestStore(t)
    initial,err:=New(store).Run(context.Background(),"rev",cfg,"root","read",map[string]any{},false)
    if err==nil || initial.Status!="suspended" || initial.Checkpoint==nil {
        t.Fatalf("expected durable sibling suspension: %#v %v",initial,err)
    }
    if initial.Checkpoint.States["early"]!="completed" ||
        initial.Value== "alice@example.com" {
        t.Fatalf("early output improperly published or not persisted: %#v",initial)
    }
    // A fresh runtime restores the previously completed root output from
    // SQLite, then runs the output schema and before_response PII policy.
    resumed,err:=New(store).Resume(context.Background(),cfg,"rev",initial.RunID,"approved")
    if err!=nil || resumed.Status!="success" ||
        resumed.Value!="[EMAIL_REDACTED]" || !resumed.OutputProduced {
        t.Fatalf("prior output not released safely: %#v %v",resumed,err)
    }
    var cp Checkpoint
    if err:=store.GetSuspended(initial.RunID,&cp);err!=sql.ErrNoRows {
        t.Fatalf("finished checkpoint not consumed: %v",err)
    }
}

func TestResumeRetainsPresenceOfPriorNullOutput(t *testing.T) {
    src:=strings.Replace(persistedRootOutputFixture(),
        `value = "literal:alice@example.com"`,`value = "null"`,1)
    src=strings.ReplaceAll(src,`output_schema = {type = "string"}`,
        `output_schema = {type = "null"}`)
    cfg,err:=config.Decode(src)
    if err!=nil {t.Fatal(err)}
    store:=effectTestStore(t)
    initial,err:=New(store).Run(context.Background(),"rev",cfg,"root","read",map[string]any{},false)
    if err==nil || initial.Status!="suspended" {t.Fatalf("expected suspend: %#v %v",initial,err)}
    finished,err:=New(store).Resume(context.Background(),cfg,"rev",initial.RunID,nil)
    if err!=nil || finished.Status!="success" || !finished.OutputProduced || finished.Value!=nil {
        t.Fatalf("prior null output lost presence or incorrectly rewritten: %#v %v",finished,err)
    }
}
