package storage

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestVerifiedBackupAuditAnchorRejectsRewritesAndTruncation(t *testing.T) {
	dir := t.TempDir()
	store, err := New(filepath.Join(dir, "leader"), initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	capture := func(store *Store, name, action string) (string, BackupManifest) {
		t.Helper()
		if err := store.AppendAudit("management", action, "succeeded"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		manifest, err := store.BackupVerified(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, manifest
	}
	firstPath, first := capture(store, "first.sqlite", "original")
	nextPath, next := capture(store, "next.sqlite", "append_only")
	anchor := AuditAnchor{Sequence: 1, SHA256: first.AuditHeadSHA256}
	if verified, err := VerifyAnchoredBackup(nextPath, anchor); err != nil || verified != next {
		t.Fatalf("valid audit continuation was refused: %v", err)
	}
	if _, err := VerifyAnchoredBackup(firstPath, AuditAnchor{Sequence: 2, SHA256: next.AuditHeadSHA256}); !errors.Is(err, ErrAuditAnchorMismatch) {
		t.Fatalf("truncated chain was not refused: %v", err)
	}

	rewrite, err := New(filepath.Join(dir, "rewritten"), initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer rewrite.Close()
	forkPath, _ := capture(rewrite, "fork.sqlite", "different_history")
	if _, err := VerifyAnchoredBackup(forkPath, anchor); !errors.Is(err, ErrAuditAnchorMismatch) {
		t.Fatalf("independently valid chain was not refused: %v", err)
	}
}
