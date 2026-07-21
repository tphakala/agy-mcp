package mcptools

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/manager"
)

// connect wires a NewServer(mgr) to a fresh client over in-memory transports
// and returns the client session.
//
// This lives in an untagged file rather than beside the posix job-supervision
// tests that first used it: nothing in it is platform-specific, and the
// cross-platform tool tests (which must stay green on Windows) need it too.
func connect(t *testing.T, mgr *manager.Manager, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	srv := NewServer(mgr)
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(t.Context(), st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, opts)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Errorf("close client session: %v", err)
		}
	})
	return cs
}
