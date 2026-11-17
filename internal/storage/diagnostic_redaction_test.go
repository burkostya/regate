package storage

import (
    "strings"
    "testing"
)

func TestSQLiteSanitizesTraceDiagnosticsAtDurableWriteBoundary(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil {t.Fatal(err)}
    defer s.Close()

    untrusted:="remote error Bearer token-that-must-not-appear user alice@example.com password=private-value"
    s.WriteTrace(Trace{RunID:"trace-run",Revision:"rev",Surface:"public",Tool:"run",Node:"remote",Status:"failure",Message:untrusted})
    saved:=s.Traces()
    if len(saved)!=1 || saved[0].RunID!="trace-run" || saved[0].Status!="failure" {
        t.Fatalf("missing durable trace identity/status: %#v",saved)
    }
    for _,secret:=range []string{"token-that-must-not-appear","alice@example.com","private-value"} {
        if strings.Contains(saved[0].Message,secret) {
            t.Fatalf("unredacted %q in stored trace: %s",secret,saved[0].Message)
        }
    }
    var raw string
    if err:=s.db.QueryRow("SELECT record FROM traces ORDER BY seq DESC LIMIT 1").Scan(&raw);err!=nil {
        t.Fatal(err)
    }
    if strings.Contains(raw,"alice@example.com") || strings.Contains(raw,"token-that-must-not-appear") {
        t.Fatal("raw SQLite trace blob leaked upstream credential or PII")
    }

    s.WriteTrace(Trace{RunID:"large",Status:"failure",Message:strings.Repeat("X",100_000)})
    got:=s.Traces()
    if len(got)!=2 || len(got[0].Message)>4096 {
        t.Fatalf("unbounded trace record: %d items, %d bytes",len(got),len(got[0].Message))
    }
}

func TestSQLiteSanitizesEffectReasonAndResolutionEvidence(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil {t.Fatal(err)}
    defer s.Close()

    untrusted:="upstream returned Bearer leaked-api-token bob@example.org password=traced-secret"
    resolution:=&EffectResolution{
        Kind:"operator",Verdict:"absent",Verifier:"operator",
        Reason:untrusted,
    }
    event:=Effect{
        ID:ID(),RunID:"effect-run",Node:"write",Operation:"crm.create",
        Status:"resolved_absent",IntentDigest:"intent-proof",
        Reason:untrusted,Resolution:resolution,
    }
    if err:=s.Effect(event);err!=nil {t.Fatal(err)}
    stored,err:=s.EffectGet(event.ID)
    if err!=nil {t.Fatal(err)}
    if stored.Status!=event.Status || stored.IntentDigest!=event.IntentDigest ||
        stored.Resolution==nil || stored.Resolution.Verdict!="absent" {
        t.Fatalf("redaction altered effect evidence identity: %#v",stored)
    }
    for _,secret:=range []string{"leaked-api-token","bob@example.org","traced-secret"} {
        if strings.Contains(stored.Reason,secret) ||
            strings.Contains(stored.Resolution.Reason,secret) {
            t.Fatalf("unredacted %q persisted in effect event",secret)
        }
    }
    if resolution.Reason!=untrusted {
        t.Fatal("storage modified the caller-owned resolution pointer")
    }
    var raw string
    if err:=s.db.QueryRow("SELECT record FROM effect_events WHERE effect_id=?",event.ID).Scan(&raw);err!=nil {
        t.Fatal(err)
    }
    if strings.Contains(raw,"leaked-api-token") || strings.Contains(raw,"bob@example.org") {
        t.Fatal("raw SQLite effect journal blob leaked credentials or PII")
    }
}
