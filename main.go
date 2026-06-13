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
	// One source of the "agy-mcp: " log prefix, set here before either the serve or
	// the run-job (supervisor) path runs, so every log line is tagged consistently
	// and individual messages no longer hardcode the prefix. This does not affect
	// errors.New strings (e.g. proc.ErrUnsupported) or the fmt.Fprintf stderr writes
	// below, which are surfaced to callers rather than logged.
	log.SetPrefix("agy-mcp: ")

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
	httpAddr := flag.String("http", "", "serve over Streamable HTTP on this address (e.g. 127.0.0.1:8765) instead of stdio; bind localhost only")
	httpToken := flag.String("http-token", "", "require Authorization: Bearer <token> in HTTP mode (overrides AGY_MCP_HTTP_TOKEN; pass an empty value to force unauthenticated)")
	flag.Parse()

	cfg, err := config.Resolve()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	// An explicitly provided -http-token (even an empty one) overrides the
	// env-resolved token, so the flag can both set and force-disable auth. An unset
	// flag leaves the AGY_MCP_HTTP_TOKEN value untouched. flag.Visit reports only the
	// flags actually present on the command line, which is how we tell "-http-token \"\""
	// (explicit disable) apart from an omitted flag. Parsing flags before Resolve keeps
	// -h working even when agy is not on PATH.
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "http-token" {
			cfg.HTTPToken = *httpToken
		}
	})
	mgr := manager.New(cfg)
	// Run startup garbage collection and concurrency-gate restoration in one
	// job-store scan (RestoreAndCollect), instead of two back-to-back scans that
	// each opened every meta.json and exit_code. It fails closed: serving with an
	// unrestored gate could bypass the cap and re-expose the agy session-lock hang
	// the gate prevents, so a scan failure refuses to start rather than logging on.
	removed, err := mgr.RestoreAndCollect()
	if err != nil {
		return fmt.Errorf("startup recovery: %w", err)
	}
	if len(removed) > 0 {
		log.Printf("startup GC removed %d expired job(s)", len(removed))
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
		authNote := "unauthenticated"
		if cfg.HTTPToken != "" {
			authNote = "bearer-token auth enabled"
		}
		log.Printf("serving Streamable HTTP on %s (%s)", *httpAddr, authNote)
		if err := mcptools.ServeHTTP(ctx, mgr, *httpAddr, cfg.HTTPToken); err != nil {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	}

	log.Print("serving over stdio")
	if err := mcptools.ServeStdio(ctx, mgr); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("stdio serve: %w", err)
	}
	return nil
}

// checkLoopbackAddr rejects any HTTP bind address that is not loopback. HTTP mode
// binds loopback only as a safety default: the bearer token is optional, so a
// non-loopback bind could expose an unauthenticated server. This refuses to do so
// rather than relying on the user reading the docs (set -http-token to add auth, but
// the loopback restriction holds regardless).
//
// A hostname is re-resolved by net.Listen at bind time, so a hosts-file or DNS
// change between this check and the bind could in principle bind non-loopback (a
// TOCTOU). That is not defended here: changing /etc/hosts or DNS requires root,
// and a root attacker can bypass any loopback restriction anyway. This check
// guards against an accidental non-loopback bind, not a privileged attacker.
func checkLoopbackAddr(addr string) error {
	return checkLoopbackAddrResolved(addr, net.LookupHost)
}

// checkLoopbackAddrResolved is checkLoopbackAddr with the name resolver injected so
// it is testable without DNS. A literal IP is checked directly; a hostname (such as
// "localhost") is resolved and EVERY resolved address must be loopback, so a
// hosts-file remap of "localhost" to a routable IP cannot silently expose the
// server (the bare-string "localhost" check this replaces trusted the name blindly).
func checkLoopbackAddrResolved(addr string, lookup func(string) ([]string, error)) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port; treat the whole value as the host
	}
	// A bracketed IPv6 literal with no port (e.g. "[::1]") keeps its brackets after
	// the no-port fallback above; strip them so net.ParseIP recognizes it (with a
	// port, SplitHostPort already removed them).
	if len(host) >= 2 && host[0] == '[' && host[len(host)-1] == ']' {
		host = host[1 : len(host)-1]
	}
	if host == "" {
		return fmt.Errorf("http address %q binds all interfaces; specify a loopback host (e.g. 127.0.0.1:8765) for HTTP mode", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("http address %q must be loopback only (localhost, 127.0.0.1, or ::1) for HTTP mode", addr)
	}
	// A hostname: resolve it and require every address to be loopback.
	addrs, err := lookup(host)
	if err != nil {
		return fmt.Errorf("resolve http host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("http host %q resolves to no addresses; specify a loopback host for HTTP mode", host)
	}
	for _, a := range addrs {
		ip := net.ParseIP(a)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("http host %q resolves to non-loopback address %s; specify a loopback host (e.g. 127.0.0.1:8765) for HTTP mode", host, a)
		}
	}
	return nil
}
