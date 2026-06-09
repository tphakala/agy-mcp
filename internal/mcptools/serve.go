package mcptools

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

func parseDuration(s string) (time.Duration, error) { return time.ParseDuration(s) }

// ServeStdio runs the server over stdio until the client disconnects or ctx is
// cancelled. stdout is the JSON-RPC stream; callers must keep all logging on
// stderr.
func ServeStdio(ctx context.Context, mgr *manager.Manager) error {
	return NewServer(mgr).Run(ctx, &mcp.StdioTransport{})
}

// HTTPHandler returns an http.Handler that serves the agy tools over the
// Streamable HTTP transport. The same manager backs every session.
func HTTPHandler(mgr *manager.Manager) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return NewServer(mgr)
	}, nil)
}

// ServeHTTP runs the Streamable HTTP server on addr until ctx is cancelled.
func ServeHTTP(ctx context.Context, mgr *manager.Manager, addr string) error {
	srv := &http.Server{Addr: addr, Handler: HTTPHandler(mgr)}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
