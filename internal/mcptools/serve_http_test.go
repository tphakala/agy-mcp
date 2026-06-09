package mcptools

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/internal/config"
	"github.com/tphakala/agy-mcp/internal/manager"
)

func TestHTTPServeListsTools(t *testing.T) {
	mgr := manager.New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, DefaultTimeout: time.Minute})
	handler := HTTPHandler(mgr)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ctx := t.Context()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) < 5 {
		t.Fatalf("expected >=5 tools, got %d", len(tools.Tools))
	}
}
