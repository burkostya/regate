package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestObserverCannotPublishAfterNewLeaderPromotes(t *testing.T) {
	dir := t.TempDir()
	first, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	revision, _, err := first.Active()
	if err != nil {
		t.Fatal(err)
	}
	draft, err := first.DraftCreate()
	if err != nil {
		t.Fatal(err)
	}
	next, err := New(dir, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if _, err := next.Activate(draft.ID); !errors.Is(err, ErrExecutionLeaseHeld) {
		t.Fatalf("observer could publish a draft: %v", err)
	}
	if _, err := first.db.Exec("UPDATE execution_leases SET deadline_ns=0"); err != nil {
		t.Fatal(err)
	}
	if err := next.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Activate(draft.ID); !errors.Is(err, ErrExecutionLeaseLost) {
		t.Fatalf("previous leader could activate after takeover: %v", err)
	}
	if _, err := first.Rollback(revision.ID); !errors.Is(err, ErrExecutionLeaseLost) {
		t.Fatalf("previous leader could roll back after takeover: %v", err)
	}
	if published, err := next.Activate(draft.ID); err != nil || published.ID == revision.ID {
		t.Fatalf("promoted leader could not publish the draft: %v", err)
	}
}

func TestStoreOpensRelativeStateDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "state")
	relative, err := filepath.Rel(cwd, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(relative, initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := os.Stat(filepath.Join(dir, databaseName)); err != nil {
		t.Fatal(err)
	}
}
