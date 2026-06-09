// Command agy-mcp is an MCP server wrapping the Antigravity CLI (agy).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
	"github.com/tphakala/agy-mcp/internal/mcptools"
	"github.com/tphakala/agy-mcp/internal/supervisor"
)

func main() {
	// stdout is reserved for the JSON-RPC stream in stdio mode. Force the
	// standard logger to stderr so a stray log line cannot corrupt the protocol.
	log.SetOutput(os.Stderr)

	if len(os.Args) >= 2 && os.Args[1] == "run-job" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: agy-mcp run-job <jobDir>")
			os.Exit(2)
		}
		if err := supervisor.Run(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "run-job: %v\n", err)
			os.Exit(1)
		}
		return
	}

	httpAddr := flag.String("http", "", "serve over Streamable HTTP on this address (e.g. 127.0.0.1:8765) instead of stdio; unauthenticated, bind localhost only")
	flag.Parse()

	cfg, err := config.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	mgr := manager.New(cfg)
	if removed, err := mgr.GarbageCollect(); err != nil {
		log.Printf("startup GC: %v", err)
	} else if len(removed) > 0 {
		log.Printf("startup GC removed %d expired job(s)", len(removed))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *httpAddr != "" {
		log.Printf("agy-mcp serving Streamable HTTP on %s", *httpAddr)
		if err := mcptools.ServeHTTP(ctx, mgr, *httpAddr); err != nil {
			fmt.Fprintf(os.Stderr, "http serve: %v\n", err)
			os.Exit(1)
		}
		return
	}

	log.Print("agy-mcp serving over stdio")
	if err := mcptools.ServeStdio(ctx, mgr); err != nil && err != io.EOF {
		fmt.Fprintf(os.Stderr, "stdio serve: %v\n", err)
		os.Exit(1)
	}
}
