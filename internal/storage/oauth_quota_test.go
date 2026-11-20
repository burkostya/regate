package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSharedOAuthQuotaReclaimsFullBuckets(t *testing.T) {
	s, err := New(t.TempDir(), initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const firstBucket = `WITH RECURSIVE n(i) AS (
		SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?
	) INSERT INTO oauth_quota(principal,ready_ns) SELECT CAST(printf('%032d',i) AS BLOB),? FROM n`
	if _, err := s.db.Exec(firstBucket, maxOAuthQuotaPrincipals, time.Now().Add(time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	admit := func() time.Duration {
		t.Helper()
		delay, err := s.AdmitOAuthRequest(context.Background(), key, time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		return delay
	}
	if delay := admit(); delay <= 0 {
		t.Fatal("new principal exceeded the shared storage bound")
	}
	if _, err := s.db.Exec("UPDATE oauth_quota SET ready_ns=0 WHERE principal=CAST(printf('%032d',1) AS BLOB)"); err != nil {
		t.Fatal(err)
	}
	if delay := admit(); delay != 0 {
		t.Fatalf("idle quota entry did not free capacity, retry in %s", delay)
	}
	if delay := admit(); delay <= 0 {
		t.Fatal("exhausted principal did not receive a retry delay")
	}
}

func TestVerifyBackupKeepsSchemaSixCompatibility(t *testing.T) {
	s, err := New(t.TempDir(), initSource())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	archive := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if _, err := s.BackupVerified(archive); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", archive)
	if err != nil {
		t.Fatal(err)
	}
	_, mutationErr := db.Exec("DROP TABLE oauth_quota; PRAGMA user_version=6")
	closeErr := db.Close()
	if mutationErr != nil || closeErr != nil {
		t.Fatalf("construct schema six fixture: %v %v", mutationErr, closeErr)
	}
	if manifest, err := VerifyBackup(archive); err != nil || manifest.SchemaVersion != 6 {
		t.Fatalf("schema six backup verification: schema=%d err=%v", manifest.SchemaVersion, err)
	}
}
