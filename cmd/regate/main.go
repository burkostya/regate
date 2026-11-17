package main

import (
	"context"
	"flag"
    "encoding/json"
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
    verifyBackup:=flag.String("verify-backup","","verify standalone SQLite backup and print non-secret JSON manifest")
    restoreBackup:=flag.String("restore-backup","","restore a verified snapshot into a pre-existing empty private -state directory")
    verifyBackupKeys:=flag.String("verify-backup-keys","","authenticate encrypted queued inputs and grants in a standalone backup using environment keyrings")
	flag.Parse()
    commands:=0
    for _,path:=range []string{*verifyBackup,*restoreBackup,*verifyBackupKeys}{
        if path!="" {commands++}
    }
    if commands>1 {
        log.Fatal("only one of -verify-backup, -verify-backup-keys, -restore-backup may be used")
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
	srv, e := server.New(*state, string(b))
	if e != nil {
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if e = srv.Serve(ctx, addr); e != nil {
        _=srv.Store.Close()
		log.Fatal(e)
	}
}
