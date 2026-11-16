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

func main() {
	file := flag.String("config", "examples/regate.hcl", "HCL configuration source")
	state := flag.String("state", ".state", "persistent state directory")
	listen := flag.String("listen", "", "override HTTP listen address")
    verifyBackup:=flag.String("verify-backup","","verify standalone SQLite backup and print non-secret JSON manifest")
	flag.Parse()
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
