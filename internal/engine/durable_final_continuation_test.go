package engine

import (
    "context"
    "database/sql"
    "strings"
    "testing"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/storage"
)

// Simulate a worker dying just after an accepted final reply was committed
// but before executing the remaining pipeline. A new runtime must recover
// with the ORIGINAL answer and must not replay the already confirmed write.
func TestRecoverDurablyAcceptedFinalContinuationAfterRestart(t *testing.T) {
    upstream, calls := testEffectUpstream(t, false)
    defer upstream.Close()
    source := nestedSuspensionSource(upstream.URL)
    cfg, err := config.Decode(source)
    if err != nil { t.Fatal(err) }
    dir := t.TempDir()
    open := func() *storage.Store {
        s, err := storage.New(dir, source)
        if err != nil { t.Fatal(err) }
        if err := s.AcquireExecutionLease(); err != nil { t.Fatal(err) }
        return s
    }
    before := open()
    initial, err := New(before).Run(context.Background(), "rev", cfg,
        "nested", "write", map[string]any{"id":"continuation"}, false)
    if err == nil || initial.Status != "suspended" || calls.Load() != 1 {
        t.Fatalf("setup failed: %#v %v calls=%d", initial, err, calls.Load())
    }
    var cp Checkpoint
    if err := before.GetSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    if err := completeSuspendedCheckpoint(cfg, "outer", &cp, initial.RunID,
        "rev", "nested", "write", []string{"call", "wait"}, "original", 1); err != nil {
        t.Fatalf("accept final answer: %v", err)
    }
    if len(PendingSuspensionPaths(&cp)) != 0 {
        t.Fatalf("final accepted reply still pending: %#v", cp)
    }
    if err := before.PutSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    if err := before.Close(); err != nil { t.Fatal(err) }

    after := open()
    defer after.Close()
    if _, err := New(after).ResumeAt(context.Background(), cfg, "rev",
        initial.RunID, []string{"call", "wait"}, "replacement"); err == nil ||
        !strings.Contains(err.Error(), "recovery forbids new input") {
        t.Fatalf("duplicate leaf reply accepted: %v", err)
    }
    if _, err := New(after).Resume(context.Background(), cfg, "rev",
        initial.RunID, "replacement"); err == nil ||
        !strings.Contains(err.Error(), "recovery forbids new input") {
        t.Fatalf("new leaf answer replaced committed one: %v", err)
    }
    // Even a completed checkpoint is not trusted without runtime validation.
    if err := after.GetSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    original := cp.Child.Values["wait"]
    cp.Child.Values["wait"] = map[string]any{"value": 17, "status": "success"}
    if err := after.PutSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    if _, err := New(after).Resume(context.Background(), cfg, "rev", initial.RunID, nil);
        err == nil || !strings.Contains(err.Error(), "completed suspend leaf failed output schema") {
        t.Fatalf("tampered final answer accepted: %v", err)
    }
    cp.Child.Values["wait"] = original
    if err := after.PutSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }

    resumed, err := New(after).Resume(context.Background(), cfg, "rev", initial.RunID, nil)
    if err != nil || resumed.Status != "success" || resumed.Value != "original" {
        t.Fatalf("restart did not drain durably accepted answer: %#v %v", resumed, err)
    }
    if calls.Load() != 1 { t.Fatalf("prior confirmed write replayed: %d", calls.Load()) }
    if err := after.GetSuspended(initial.RunID, &cp); err != sql.ErrNoRows {
        t.Fatalf("consumed checkpoint remains: %v", err)
    }
}

// A deterministic failure AFTER acceptance must preserve the final answer,
// so the caller can never replace it while attempting to recover the graph.
func TestLastContinuationIsCommittedBeforeDownstreamFailure(t *testing.T) {
    upstream, calls := testEffectUpstream(t, false)
    defer upstream.Close()
    source := strings.Replace(nestedSuspensionSource(upstream.URL),
        `value = "nodes.call.value"`, `value = "nodes.call.value.nonexistent"`, 1)
    cfg, err := config.Decode(source)
    if err != nil { t.Fatal(err) }
    store := effectTestStore(t)
    initial, err := New(store).Run(context.Background(), "rev", cfg,
        "nested", "write", map[string]any{"id":"failure"}, false)
    if err == nil || initial.Status != "suspended" || calls.Load() != 1 {
        t.Fatalf("setup failed: %#v %v calls=%d", initial, err, calls.Load())
    }
    failed, err := New(store).Resume(context.Background(), cfg, "rev", initial.RunID, "accepted")
    if err == nil || failed.Status != "failure" {
        t.Fatalf("expected downstream expression failure: %#v %v", failed, err)
    }
    var cp Checkpoint
    if err := store.GetSuspended(initial.RunID, &cp); err != nil { t.Fatal(err) }
    if pending := PendingSuspensionPaths(&cp); len(pending) != 0 {
        t.Fatalf("previously accepted leaf returned to pending: %#v", pending)
    }
    child := cp.Child
    if child == nil { t.Fatalf("child checkpoint lost: %#v", cp) }
    state, _ := child.Values["wait"].(map[string]any)
    if state["value"] != "accepted" {
        t.Fatalf("final answer not durably retained: %#v", state)
    }
    if _, err := New(store).Resume(context.Background(), cfg, "rev", initial.RunID, "changed");
        err == nil || !strings.Contains(err.Error(), "recovery forbids new input") {
        t.Fatalf("old reply overwritten after downstream failure: %v", err)
    }
    if _, err := New(store).Resume(context.Background(), cfg, "rev", initial.RunID, nil); err == nil {
        t.Fatal("downstream failure unexpectedly recovered")
    }
    if calls.Load() != 1 { t.Fatalf("recovery replayed remote write: %d", calls.Load()) }
}
