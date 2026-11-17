package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineBackupKeyAuditAuthenticatesQueueAndGrant(t *testing.T) {
	grantKey := testGrantKey(t)
	queueKey := testGrantKey(t)
	testGrantEnv(t, "grant", map[string]string{"grant": grantKey})
	setQueueRotationRing(t, "queue", map[string]string{"queue": queueKey})
	s, err := New(t.TempDir(), initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.AcquireExecutionLease(); err != nil {
		t.Fatal(err)
	}
	busy := capacityTask(s, "working")
	if _, err := s.AdmitOrQueueTask(context.Background(), busy,
		map[string]any{"id": "busy"}, 1, 2); err != nil {
		t.Fatal(err)
	}
	queued := capacityTask(s, "working")
	if deferred, err := s.AdmitOrQueueTask(context.Background(), queued,
		map[string]any{"private": "queue-secret-not-in-report"}, 1, 2); err != nil || !deferred {
		t.Fatalf("encrypted queue fixture failed: %v", err)
	}
	delegated := testGrantTask(t, s)
	if _, err := s.CreateTaskGrant(context.Background(), delegated.TaskID,
		delegated.OwnerDigest, "grant-secret-not-in-report", expiresGrant()); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	manifest, err := s.BackupVerified(archive)
	if err != nil {
		t.Fatal(err)
	}
	report, err := VerifyBackupKeys(archive)
	if err != nil {
		t.Fatal(err)
	}
	if report.SnapshotSHA256 != manifest.SHA256 ||
		report.QueuedAuthenticated != 1 ||
		report.GrantsAuthenticated != 1 || report.GrantsNonExecutable != 0 {
		t.Fatalf("unexpected key audit report: %#v", report)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{grantKey, queueKey,
		"queue-secret-not-in-report", "grant-secret-not-in-report",
		queued.TaskID, delegated.OwnerDigest} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("offline key report revealed sensitive material")
		}
	}
	// The keyring is not an optional convenience: every retained live
	// ciphertext needs its original key, not merely a key with matching ID.
	t.Setenv("REGATE_GRANT_KEYS_JSON", "")
	if _, err := VerifyBackupKeys(archive); !errors.Is(err, ErrGrantKeyUnavailable) {
		t.Fatalf("missing grant key not detected: %v", err)
	}
	testGrantEnv(t, "grant", map[string]string{"grant": testGrantKey(t)})
	if _, err := VerifyBackupKeys(archive); !errors.Is(err, ErrGrantKeyUnavailable) {
		t.Fatalf("wrong grant key not detected: %v", err)
	}
	testGrantEnv(t, "grant", map[string]string{"grant": grantKey})
	t.Setenv(taskQueueKeyEnv, "")
	if _, err := VerifyBackupKeys(archive); !errors.Is(err, ErrTaskQueueKeyUnavailable) {
		t.Fatalf("missing queue key not detected: %v", err)
	}
	setQueueRotationRing(t, "queue", map[string]string{"queue": testGrantKey(t)})
	if _, err := VerifyBackupKeys(archive); !errors.Is(err, ErrTaskQueueKeyUnavailable) {
		t.Fatalf("wrong queue key not detected: %v", err)
	}
}

func TestOfflineBackupKeyAuditRejectsAuthenticatedMetadataDrift(t *testing.T) {
	testGrantEnv(t, "grant", map[string]string{"grant": testGrantKey(t)})
	setQueueKeyRing(t)
	s := queueStore(t)
	busy := capacityTask(s, "working")
	if _, err := s.AdmitOrQueueTask(context.Background(), busy,
		map[string]any{"id": "busy"}, 1, 2); err != nil {
		t.Fatal(err)
	}
	queued := capacityTask(s, "working")
	if _, err := s.AdmitOrQueueTask(context.Background(), queued,
		map[string]any{"private": "hidden"}, 1, 2); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.Backup(archive); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE task_queue SET surface=? WHERE task_id=?",
		"unauthorized-surface", queued.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A structurally intact SQLite page is not necessarily a valid
	// authenticated task envelope.
	if _, err := VerifyBackup(archive); err != nil {
		t.Fatalf("structurally valid test fixture failed: %v", err)
	}
	if _, err := VerifyBackupKeys(archive); !errors.Is(err, ErrBackupKeyAudit) {
		t.Fatalf("modified encrypted envelope was accepted: %v", err)
	}
}

func TestOfflineBackupKeyAuditDoesNotRequireKeysForRevokedGrant(t *testing.T) {
	testGrantEnv(t, "grant", map[string]string{"grant": testGrantKey(t)})
	t.Setenv(taskQueueKeyEnv, "")
	s := newGrantStore(t)
	task := testGrantTask(t, s)
	if _, err := s.CreateTaskGrant(context.Background(), task.TaskID,
		task.OwnerDigest, "revoked-secret", expiresGrant()); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTaskGrant(context.Background(), task.TaskID, task.OwnerDigest); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "revoked.sqlite")
	if err := s.Backup(archive); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGATE_GRANT_KEYS_JSON", "")
	report, err := VerifyBackupKeys(archive)
	if err != nil || report.GrantsNonExecutable != 1 ||
		report.GrantsAuthenticated != 0 {
		t.Fatalf("revoked grant required a retired key: %#v %v", report, err)
	}
}

func TestOfflineBackupKeyAuditRequiresValidStandaloneSnapshot(t *testing.T) {
	t.Setenv("REGATE_GRANT_KEYS_JSON", "")
	t.Setenv(taskQueueKeyEnv, "")
	broken := filepath.Join(t.TempDir(), "bad.sqlite")
	if err := os.WriteFile(broken, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackupKeys(broken); !errors.Is(err, ErrInvalidBackup) {
		t.Fatalf("corrupt archive was accepted: %v", err)
	}
}
