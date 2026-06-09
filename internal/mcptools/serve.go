package mcptools

import (
	"context"
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
