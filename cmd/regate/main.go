package main

import (
	"context"
	"flag"
    "encoding/json"
	"errors"
	"fmt"
	"log"
    "io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/burkostya/regate/internal/server"
    "github.com/burkostya/regate/internal/storage"
)

func verifyBackupToWriter(path string,w io.Writer)error {
    manifest,err:=storage.VerifyBackup(path)
    if err!=nil{return err}
    return json.NewEncoder(w).Encode(manifest)
}

func verifyBackupKeysToWriter(path string, w io.Writer) error {
	report, err := storage.VerifyBackupKeys(path)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(report)
}

func acknowledgeRestoreToWriter(dir, sha, actor, evidence string, w io.Writer) error {
	receipt,err:=storage.AcknowledgeRestoredState(dir,sha,actor,evidence)
	if err!=nil{return err}
	return json.NewEncoder(w).Encode(receipt)
}

func restoreBackupToWriter(archive, stateDir string, w io.Writer) error {
	manifest, err := storage.RestoreVerifiedBackup(archive, stateDir)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(manifest)
}

func main() {
	file := flag.String("config", "examples/regate.hcl", "HCL configuration source")
	state := flag.String("state", ".state", "persistent state directory")
	listen := flag.String("listen", "", "override HTTP listen address")
	waitForLeader := flag.Bool("wait-for-leader", false, "wait for the existing worker to release the shared SQLite execution lease")
    verifyBackup:=flag.String("verify-backup","","verify standalone SQLite backup and print non-secret JSON manifest")
    restoreBackup:=flag.String("restore-backup","","restore a verified snapshot into a pre-existing empty private -state directory")
    verifyBackupKeys:=flag.String("verify-backup-keys","","authenticate encrypted queued inputs and grants in a standalone backup using environment keyrings")
    ackRestore:=flag.Bool("acknowledge-restore",false,"OFFLINE: discard historical executable work and lift restored-state quarantine (irreversible)")
    ackSHA:=flag.String("snapshot-sha256","","exact SHA-256 digest from the restored backup manifest")
    ackActor:=flag.String("operator","","identity of the responsible recovery operator")
    ackEvidence:=flag.String("fence-evidence","","operator statement of old-worker and upstream isolation; stored as digest only")
	flag.Parse()
    commands:=0
    for _,path:=range []string{*verifyBackup,*restoreBackup,*verifyBackupKeys}{
        if path!="" {commands++}
    }
    if *ackRestore {commands++}
    if commands>1 {
        log.Fatal("only one offline backup/restore command may be used")
    }
    if *ackRestore {
        stateExplicit:=false
        flag.Visit(func(f *flag.Flag){if f.Name=="state" {stateExplicit=true}})
        if !stateExplicit || *ackSHA=="" || *ackActor=="" || *ackEvidence==""{
            log.Fatal("-acknowledge-restore requires -state, -snapshot-sha256, -operator and -fence-evidence")
        }
        if err:=acknowledgeRestoreToWriter(*state,*ackSHA,*ackActor,*ackEvidence,os.Stdout);err!=nil{
            log.Fatal(err)
        }
        return
    }
    if *restoreBackup!="" {
        stateExplicit:=false
        flag.Visit(func(f *flag.Flag){if f.Name=="state" {stateExplicit=true}})
        if !stateExplicit {
            log.Fatal("-restore-backup requires an explicit -state destination")
        }
        if err:=restoreBackupToWriter(*restoreBackup,*state,os.Stdout);err!=nil {
            log.Fatal(err)
        }
        return
    }
    if *verifyBackupKeys!="" {
        if err:=verifyBackupKeysToWriter(*verifyBackupKeys,os.Stdout);err!=nil {
            log.Fatal(err)
        }
        return
    }
    if *verifyBackup!="" {
        // Verification is intentionally read-only and works without a
        // running Regate service, admin bearer or live configuration.
        if err:=verifyBackupToWriter(*verifyBackup,os.Stdout);err!=nil {
            log.Fatal(err)
        }
        return
    }
	b, e := os.ReadFile(*file)
	if e != nil {
		log.Fatal(e)
	}
    // Fail before acquiring SQLite execution leadership if essential admin
    // authentication is not configured. log.Fatal calls os.Exit and bypasses
    // defer, so fatal paths after startup must release the lease explicitly.
    if os.Getenv("REGATE_ADMIN_TOKEN")=="" {
        log.Fatal("REGATE_ADMIN_TOKEN is required; refusing management MCP without authentication")
    }
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	start := server.New
	if *waitForLeader {
		start = func(state, source string) (*server.Service, error) {
			return server.WaitForLeader(ctx, state, source)
		}
	}
	srv, e := start(*state, string(b))
	if e != nil {
		if errors.Is(e, context.Canceled) && ctx.Err() != nil {
			return
		}
		log.Fatal(e)
	}
    defer srv.Store.Close()
	cfg, _ := srv.Current()
	addr := cfg.Listen
	if *listen != "" {
		addr = *listen
	}
	host, _, e := net.SplitHostPort(addr)
	if e != nil {
        _=srv.Store.Close()
		log.Fatal(e)
	}
	
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && os.Getenv("REGATE_PUBLIC_TOKEN") == "" {
        _=srv.Store.Close()
		log.Fatal("non-loopback listen requires REGATE_PUBLIC_TOKEN")
	}
	log.Println(fmt.Sprintf("Regate config loaded: %s", srv.DebugConfig()))
	if e = srv.Serve(ctx, addr); e != nil {
        _=srv.Store.Close()
		log.Fatal(e)
	}
}
