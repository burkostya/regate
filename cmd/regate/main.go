package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/burkostya/regate/internal/server"
)

func main() {
	file := flag.String("config", "examples/regate.hcl", "HCL configuration source")
	state := flag.String("state", ".state", "persistent state directory")
	listen := flag.String("listen", "", "override HTTP listen address")
	flag.Parse()
	b, e := os.ReadFile(*file)
	if e != nil {
		log.Fatal(e)
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
		log.Fatal(e)
	}
	if os.Getenv("REGATE_ADMIN_TOKEN") == "" {
		log.Fatal("REGATE_ADMIN_TOKEN is required; refusing to expose management MCP without authentication")
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && os.Getenv("REGATE_PUBLIC_TOKEN") == "" {
		log.Fatal("non-loopback listen requires REGATE_PUBLIC_TOKEN")
	}
	log.Println(fmt.Sprintf("Regate config loaded: %s", srv.DebugConfig()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if e = srv.Serve(ctx, addr); e != nil {
		log.Fatal(e)
	}
}
