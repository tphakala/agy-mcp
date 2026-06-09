package manager

import "testing"

func TestExpectedComm(t *testing.T) {
	if got := expectedComm("/usr/local/bin/agy-mcp"); got != "agy-mcp" {
		t.Errorf("expectedComm basename = %q, want agy-mcp", got)
	}
	// Kernel /proc/comm is capped at 15 chars; a longer basename is truncated.
	if got := expectedComm("/x/agy-mcp-v1.2.3-linux-amd64"); got != "agy-mcp-v1.2.3-" {
		t.Errorf("expectedComm truncation = %q (len %d)", got, len(got))
	}
}
