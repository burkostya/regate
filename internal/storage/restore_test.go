package storage

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func privateEmptyDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "restored")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRestoreVerifiedBackupCopiesAllStateWithoutStartingWorker(t *testing.T) {
	source := "regate { listen = \"127.0.0.1:8080\" }"
	s, err := New(t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	expected, err := s.BackupVerified(archive)
	if err != nil {
		t.Fatal(err)
	}
	dir := privateEmptyDir(t)
	actual, err := RestoreVerifiedBackup(archive, dir)
	if err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("restore manifest mismatch: got %#v want %#v", actual, expected)
	}
	installed, err := VerifyBackup(filepath.Join(dir, databaseName))
	if err != nil || installed != expected {
		t.Fatalf("installed snapshot invalid: %#v %v", installed, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != databaseName {
		t.Fatalf("restore exposed partial files: %#v %v", entries, err)
	}
	// Assert the byte digest before any SQLite observer legitimately changes
	// journal metadata on opening the restored database.
	if _, err := RestoreVerifiedBackup(archive, dir); !errors.Is(err, ErrUnsafeRestoreTarget) {
		t.Fatalf("second restore was not rejected: %v", err)
	}
	after, err := VerifyBackup(filepath.Join(dir, databaseName))
	if err != nil || after != expected {
		t.Fatalf("second restore damaged original: %#v %v", after, err)
	}
	observer, err := New(dir, source)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if err := observer.AcquireExecutionLease(); !errors.Is(err, ErrExecutionLeaseHeld) {
		t.Fatalf("restored database bypassed original live leader: %v", err)
	}
	_, loaded, err := observer.Active()
	if err != nil || loaded != source {
		t.Fatalf("restored immutable source changed: %q %v", loaded, err)
	}
}

func TestRestoreVerifiedBackupRejectsUnsafeStateAndArchives(t *testing.T) {
	s, err := New(t.TempDir(), "regate {}")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.Backup(archive); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	missing := filepath.Join(base, "missing-state")
	if _, err := RestoreVerifiedBackup(archive, missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("nonexistent state directory implicitly created: %v", err)
	}
	nonempty := privateEmptyDir(t)
	sentinel := filepath.Join(nonempty, "do-not-touch")
	if err := os.WriteFile(sentinel, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreVerifiedBackup(archive, nonempty); !errors.Is(err, ErrUnsafeRestoreTarget) {
		t.Fatalf("existing state was accepted: %v", err)
	}
	original, err := os.ReadFile(sentinel)
	if err != nil || string(original) != "original" {
		t.Fatalf("existing state was modified: %q %v", original, err)
	}
	unsafeDir := privateEmptyDir(t)
	if err := os.Chmod(unsafeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreVerifiedBackup(archive, unsafeDir); !errors.Is(err, ErrUnsafeRestoreTarget) {
		t.Fatalf("world-readable state directory accepted: %v", err)
	}
	alias := filepath.Join(base, "state-alias")
	if err := os.Symlink(privateEmptyDir(t), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreVerifiedBackup(archive, alias); !errors.Is(err, ErrUnsafeRestoreTarget) {
		t.Fatalf("symlink state directory accepted: %v", err)
	}
	broken := filepath.Join(t.TempDir(), "corrupt.sqlite")
	if err := os.WriteFile(broken, []byte("not a SQLite backup"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := privateEmptyDir(t)
	if _, err := RestoreVerifiedBackup(broken, dir); !errors.Is(err, ErrInvalidBackup) {
		t.Fatalf("invalid archive was installed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid archive left state files: %#v %v", entries, err)
	}
}

func TestRestoreVerifiedBackupConcurrentPublishIsNoOverwrite(t *testing.T) {
	s, err := New(t.TempDir(), "regate {}")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	expected, err := s.BackupVerified(archive)
	if err != nil {
		t.Fatal(err)
	}
	dir := privateEmptyDir(t)
	const contenders = 4
	var group sync.WaitGroup
	results := make(chan error, contenders)
	for n := 0; n < contenders; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := RestoreVerifiedBackup(archive, dir)
			results <- err
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected one winner, got %d", successes)
	}
	found, err := VerifyBackup(filepath.Join(dir, databaseName))
	if err != nil || found != expected {
		t.Fatalf("raced restore changed installed backup: %#v %v", found, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed publishers left temporary state: %#v %v", entries, err)
	}
}
