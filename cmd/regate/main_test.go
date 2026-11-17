package main

import (
    "bytes"
    "encoding/json"
    "os"
    "path/filepath"
    "testing"

    "github.com/burkostya/regate/internal/storage"
)

func TestOfflineBackupVerifierWritesOnlyManifest(t *testing.T) {
    s,err:=storage.New(t.TempDir(),`regate { listen = "127.0.0.1:8080" }`)
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    destination:=filepath.Join(t.TempDir(),"verified.sqlite")
    if err=s.Backup(destination);err!=nil{t.Fatal(err)}
    // An offline verifier does not require a Regate admin bearer, HCL
    // config file or execution lease. It never runs recovery.
    t.Setenv("REGATE_ADMIN_TOKEN","")
    var output bytes.Buffer
    if err=verifyBackupToWriter(destination,&output);err!=nil{t.Fatal(err)}
    var manifest storage.BackupManifest
    if err=json.Unmarshal(output.Bytes(),&manifest);err!=nil{t.Fatal(err)}
    if manifest.Revisions!=1 || len(manifest.SHA256)!=64 ||
        manifest.ActiveRevision=="" {
        t.Fatalf("CLI manifest missing verification fields: %#v",manifest)
    }
    if bytes.Contains(output.Bytes(),[]byte("127.0.0.1:8080")) {
        t.Fatal("original HCL source leaked through backup manifest")
    }
    missing:=filepath.Join(t.TempDir(),"missing.sqlite")
    if err=verifyBackupToWriter(missing,&bytes.Buffer{});err==nil ||
        !os.IsNotExist(err){
        t.Fatalf("missing backup was not rejected: %v",err)
    }
}

func TestOfflineRestoreCLIProducesOnlyVerifiedManifest(t *testing.T) {
	s, err := storage.New(t.TempDir(), "regate {}")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	archive := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Backup(archive); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "fresh")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGATE_ADMIN_TOKEN", "")
	var output bytes.Buffer
	if err := restoreBackupToWriter(archive, dir, &output); err != nil {
		t.Fatal(err)
	}
	var manifest storage.BackupManifest
	if err := json.Unmarshal(output.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	verified, err := storage.VerifyBackup(filepath.Join(dir, "regate.sqlite"))
	if err != nil || verified != manifest {
		t.Fatalf("CLI restore did not install a verified snapshot: %#v %v", verified, err)
	}
	if bytes.Contains(output.Bytes(), []byte("regate {}")) {
		t.Fatal("restored source leaked into manifest")
	}
}

func TestOfflineKeyAuditCLISuppressesSecrets(t *testing.T) {
	s, err := storage.New(t.TempDir(), "regate {}")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	archive := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Backup(archive); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGATE_ADMIN_TOKEN", "")
	var output bytes.Buffer
	if err := verifyBackupKeysToWriter(archive, &output); err != nil {
		t.Fatal(err)
	}
	var report storage.BackupKeyAudit
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.SnapshotSHA256) != 64 ||
		report.QueuedAuthenticated != 0 || report.GrantsAuthenticated != 0 {
		t.Fatalf("offline key verification reported incorrect counts: %#v", report)
	}
	if bytes.Contains(output.Bytes(), []byte("regate {}")) {
		t.Fatal("original HCL source leaked in offline audit")
	}
}
