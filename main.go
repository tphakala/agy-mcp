// Command agy-mcp is an MCP server wrapping the Antigravity CLI (agy).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
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

	if err := serve(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

// serve resolves configuration, runs startup garbage collection, and serves the
// MCP tools over stdio (default) or Streamable HTTP. It returns an error rather
// than calling os.Exit so deferred cleanup (the signal stop) still runs.
func serve() error {
	httpAddr := flag.String("http", "", "serve over Streamable HTTP on this address (e.g. 127.0.0.1:8765) instead of stdio; unauthenticated, bind localhost only")
	flag.Parse()

	cfg, err := config.Resolve()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	mgr := manager.New(cfg)
	if removed, err := mgr.GarbageCollect(); err != nil {
		log.Printf("startup GC: %v", err)
	} else if len(removed) > 0 {
		log.Printf("startup GC removed %d expired job(s)", len(removed))
	}
	// Re-occupy the concurrency gate for jobs whose detached supervisor outlived a
	// previous manager, so a new run is serialized against them and the cap holds.
	// Fail closed: serving with an unrestored gate could bypass the cap and
	// re-expose the agy session-lock hang the gate prevents.
	if err := mgr.RestoreGate(); err != nil {
		return fmt.Errorf("restore concurrency gate: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Sweep finished jobs periodically, not just at startup, so a long-lived daemon
	// (especially HTTP serve mode) does not accumulate finished job dirs until the
	// next restart. Stops when ctx is cancelled on shutdown; no-op if JobTTL<=0.
	go mgr.RunPeriodicGCFromConfig(ctx)

	if *httpAddr != "" {
		if err := checkLoopbackAddr(*httpAddr); err != nil {
			return err
		}
		log.Printf("agy-mcp serving Streamable HTTP on %s", *httpAddr)
		if err := mcptools.ServeHTTP(ctx, mgr, *httpAddr); err != nil {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	}

	log.Print("agy-mcp serving over stdio")
	if err := mcptools.ServeStdio(ctx, mgr); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("stdio serve: %w", err)
	}
	return nil
}

// checkLoopbackAddr rejects any HTTP bind address that is not loopback. The
// HTTP mode is unauthenticated, so binding a non-loopback interface would
// expose it; this refuses to do so rather than relying on the user reading the
// docs.
func checkLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port; treat the whole value as the host
	}
	if host == "" {
		return fmt.Errorf("http address %q binds all interfaces; specify a loopback host (e.g. 127.0.0.1:8765) for the unauthenticated HTTP mode", addr)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("http address %q must be loopback only (localhost, 127.0.0.1, or ::1) for the unauthenticated HTTP mode", addr)
}
