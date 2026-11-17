package engine

import (
	"context"
	"testing"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/storage"
)

func TestNestedSuspensionCheckpointSurvivesSQLiteRestart(t *testing.T) {
	upstream, calls := testEffectUpstream(t, false)
	defer upstream.Close()
	source := nestedSuspensionSource(upstream.URL)
	cfg, err := config.Decode(source)
	if err != nil { t.Fatal(err) }
	dir := t.TempDir()
	before, err := storage.New(dir, source)
	if err != nil { t.Fatal(err) }
	if err = before.AcquireExecutionLease(); err != nil { t.Fatal(err) }
	out, err := New(before).Run(context.Background(), "rev", cfg, "nested", "write",
		map[string]any{"id": "C500"}, false)
	if err == nil || out.Status != "suspended" || calls.Load() != 1 {
		t.Fatalf("expected durable suspension before restart: %#v %v writes=%d", out, err, calls.Load())
	}
	if err = before.Close(); err != nil { t.Fatal(err) }
	after, err := storage.New(dir, source)
	if err != nil { t.Fatal(err) }
	defer after.Close()
	if err = after.AcquireExecutionLease(); err != nil { t.Fatal(err) }
	var cp Checkpoint
	if err = after.GetSuspended(out.RunID, &cp); err != nil {
		t.Fatalf("checkpoint lost after restart: %v", err)
	}
	if cp.Child == nil || cp.Child.States["create"] != "completed" {
		t.Fatalf("invalid recovered child state: %#v", cp)
	}
	resumed, err := New(after).Resume(context.Background(), cfg, "rev", out.RunID, "approved")
	if err != nil || resumed.Status != "success" || resumed.Value != "approved" {
		t.Fatalf("restart resume failed: %#v %v", resumed, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("restart replayed prior confirmed write: %d", calls.Load())
	}
}

func TestNestedSuspensionRejectsMismatchedChildIdentity(t *testing.T) {
	upstream, calls := testEffectUpstream(t, false)
	defer upstream.Close()
	cfg, err := config.Decode(nestedSuspensionSource(upstream.URL))
	if err != nil { t.Fatal(err) }
	store := effectTestStore(t)
	out, err := New(store).Run(context.Background(), "rev", cfg, "nested", "write",
		map[string]any{"id": "C501"}, false)
	if err == nil || out.Status != "suspended" { t.Fatalf("not suspended: %#v %v", out, err) }
	var cp Checkpoint
	if err := store.GetSuspended(out.RunID, &cp); err != nil { t.Fatal(err) }
	cp.Child.RunID = "mismatched-child-run"
	if err := store.PutSuspended(out.RunID, cp); err != nil { t.Fatal(err) }
	_, err = New(store).Resume(context.Background(), cfg, "rev", out.RunID, "approved")
	if err == nil { t.Fatal("tampered child run identity was accepted") }
	if calls.Load() != 1 { t.Fatalf("unexpected external replay: %d", calls.Load()) }
}
