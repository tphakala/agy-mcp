package mcptools

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

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

// ServeHTTP runs the Streamable HTTP server on addr until ctx is cancelled, then
// shuts down gracefully so in-flight responses can drain.
func ServeHTTP(ctx context.Context, mgr *manager.Manager, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           HTTPHandler(mgr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Derive a cancelable context so the shutdown goroutine cannot leak if
	// ListenAndServe returns on its own (for example, a bind failure).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(shutCtx)
	}()
	err := srv.ListenAndServe()
	cancel() // unblock the shutdown goroutine when ListenAndServe returns first
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
