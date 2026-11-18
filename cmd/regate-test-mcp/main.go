// regate-test-mcp is a standalone loopback-only MCP test provider.
// Start two independent instances with unique names and state directories.
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

	"github.com/burkostya/regate/internal/testmcp"
)

func main() {
	name := flag.String("name", "", "unique fixture identity (alpha or beta)")
	dir := flag.String("state", "", "private test-only state directory")
	addr := flag.String("listen", "127.0.0.1:9201", "loopback listen address")
	flag.Parse()
	srv, err := testmcp.New(*name, *dir)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	// A readiness line makes the same binary useful in integration scripts.
	fmt.Fprintf(os.Stdout, "READY http://%s/mcp\n", ln.Addr().String())
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := srv.Serve(ctx, ln); err != nil {
		log.Fatal(err)
	}
}
