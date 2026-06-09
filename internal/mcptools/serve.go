package mcptools

import (
	"context"
	"crypto/subtle"
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
//
// The handler is wrapped with cross-origin protection: a state-changing
// cross-origin browser POST (signalled by Sec-Fetch-Site or a mismatched Origin)
// is rejected with 403. Requests with no Origin/Sec-Fetch-Site (the normal
// non-browser MCP clients) are treated as same-origin and pass through, so this
// hardening does not affect Claude Code, Cursor, or the go-sdk client.
//
// When token is non-empty, a bearer-token check is applied in front of the
// cross-origin protection: a request without a matching Authorization: Bearer
// <token> header is rejected with 401. An empty token leaves HTTP mode
// unauthenticated (the default).
func HTTPHandler(mgr *manager.Manager, token string) http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return NewServer(mgr)
	}, nil)
	return withBearerAuth(token, http.NewCrossOriginProtection().Handler(streamable))
}

// withBearerAuth wraps h to require Authorization: Bearer <token>. An empty token
// disables the check and returns h unchanged. The comparison is constant-time so a
// timing side-channel cannot reveal the token; an unequal length short-circuits to
// a mismatch, which leaks only length, the standard tradeoff for a bearer check.
func withBearerAuth(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ServeHTTP runs the Streamable HTTP server on addr until ctx is cancelled, then
// shuts down gracefully so in-flight responses can drain. A non-empty token
// requires Authorization: Bearer <token> on every request; an empty token leaves
// HTTP mode unauthenticated.
func ServeHTTP(ctx context.Context, mgr *manager.Manager, addr, token string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           HTTPHandler(mgr, token),
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
