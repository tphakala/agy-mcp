package mcptools

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tphakala/agy-mcp/v2/internal/config"
	"github.com/tphakala/agy-mcp/v2/internal/manager"
)

func testManager(t *testing.T) *manager.Manager {
	t.Helper()
	return manager.New(config.Config{StateDir: t.TempDir(), MaxConcurrency: 4, DefaultTimeout: time.Minute,
		ConversationCacheFile: filepath.Join(t.TempDir(), "last_conversations.json")})
}

// postJSON builds a JSON POST bound to the test's context. The error is checked
// here rather than discarded at each call site: the inputs are literals today,
// so it cannot fire, but a URL or body that later becomes malformed would
// otherwise nil-deref req instead of failing the test cleanly.
func postJSON(t *testing.T, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestHTTPRejectsCrossOriginPost verifies the Streamable HTTP handler rejects a
// cross-origin browser-style POST (Sec-Fetch-Site: cross-site) with 403, the
// Origin/CSRF hardening from issue #5.
func TestHTTPRejectsCrossOriginPost(t *testing.T) {
	ts := httptest.NewServer(HTTPHandler(testManager(t), ""))
	defer ts.Close()

	req := postJSON(t, ts.URL, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST, got %d", resp.StatusCode)
	}
}

// headerRoundTripper injects a fixed header on every request, so a go-sdk client
// can present a bearer token.
type headerRoundTripper struct {
	key, val string
	base     http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(h.key, h.val)
	return h.base.RoundTrip(req)
}

// TestHTTPBearerAuth verifies the optional bearer-token middleware (issue #5):
// the correct token connects, a missing or wrong token gets 401.
func TestHTTPBearerAuth(t *testing.T) {
	ts := httptest.NewServer(HTTPHandler(testManager(t), "s3cret"))
	defer ts.Close()

	t.Run("correct token connects", func(t *testing.T) {
		authed := &http.Client{Transport: headerRoundTripper{
			key: "Authorization", val: "Bearer s3cret", base: http.DefaultTransport,
		}}
		client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
		cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: authed}, nil)
		if err != nil {
			t.Fatalf("connect with valid token: %v", err)
		}
		defer func() { _ = cs.Close() }()
		if _, err := cs.ListTools(t.Context(), nil); err != nil {
			t.Fatalf("list tools with valid token: %v", err)
		}
	})

	t.Run("missing token is 401", func(t *testing.T) {
		req := postJSON(t, ts.URL, `{}`)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("missing token: expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("wrong token is 401", func(t *testing.T) {
		req := postJSON(t, ts.URL, `{}`)
		req.Header.Set("Authorization", "Bearer wrong")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wrong token: expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("lowercase scheme accepted", func(t *testing.T) {
		// RFC 7235: the auth-scheme is case-insensitive, so "bearer <token>" with the
		// correct token must authenticate (it reaches the MCP handler, so it is never 401).
		req := postJSON(t, ts.URL, `{}`)
		req.Header.Set("Authorization", "bearer s3cret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("lowercase scheme with correct token must not be 401, got %d", resp.StatusCode)
		}
	})
}

// TestHTTPNoTokenSkipsAuth pins the default: with no token configured, a request
// carrying no Authorization header is not rejected by the auth layer (it reaches
// the MCP handler, so it is never a 401). This guards against a regression that
// would accidentally require auth even when unconfigured.
func TestHTTPNoTokenSkipsAuth(t *testing.T) {
	ts := httptest.NewServer(HTTPHandler(testManager(t), ""))
	defer ts.Close()

	req := postJSON(t, ts.URL, `{}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("no token configured: request must not be rejected with 401, got %d", resp.StatusCode)
	}
}

func TestHTTPServeListsTools(t *testing.T) {
	handler := HTTPHandler(testManager(t), "")
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
	// Assert the exact registered set, not a lower bound: a dropped or renamed
	// tool is a wire-contract regression that "len >= 5" would miss.
	got := make(map[string]bool, len(tools.Tools))
	for _, tool := range tools.Tools {
		got[tool.Name] = true
	}
	want := []string{"agy_run", "agy_status", "agy_cancel", "agy_run_sync", "agy_wait", "list_models", "list_agents", "list_sessions"}
	if len(tools.Tools) != len(want) {
		t.Fatalf("registered %d tools, want %d (%v)", len(tools.Tools), len(want), want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("tool %q is not registered", name)
		}
	}
}

// TestHTTPServeAdvertisesInstructions guards the discovery surface: the server
// must send a non-empty MCP instructions string at initialize (a regression to
// nil ServerOptions would silently drop it), and the entry-point tools must keep
// naming their use cases so a client under-reaching for them is less likely.
func TestHTTPServeAdvertisesInstructions(t *testing.T) {
	handler := HTTPHandler(testManager(t), "")
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ctx := t.Context()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("no InitializeResult")
	}
	// Spot-check anchors, not the full text: a use-case cue, the sync entry-point
	// tool name, the parallelism guidance that mirrors the gate's real scope, and
	// the prompt-injection boundary that marks a delegated result as untrusted.
	for _, want := range []string{"Peer review", "agy_run_sync", "SAME conversation_id", "report to weigh"} {
		if !strings.Contains(init.Instructions, want) {
			t.Errorf("server instructions missing %q; got:\n%s", want, init.Instructions)
		}
	}

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	desc := make(map[string]string, len(tools.Tools))
	for _, tool := range tools.Tools {
		desc[tool.Name] = tool.Description
	}
	for _, name := range []string{"agy_run", "agy_run_sync"} {
		if !strings.Contains(desc[name], "peer review") {
			t.Errorf("tool %q description no longer names a use case: %q", name, desc[name])
		}
	}
}
