package manager

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestReadFileContract pins what readFile's three callers rely on, none of which
// tested it directly before its read was pre-sized: a missing file is an empty
// result rather than an error (an absent answer must not be reported as a
// failure), the trailing newline is trimmed so a payload response and a streamed
// out read identically, and interior newlines survive.
func TestReadFileContract(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		write   bool
		content string
		want    string
	}{
		{name: "missing file reads empty, not an error"},
		{name: "empty file", write: true},
		{name: "trailing newlines trimmed", write: true, content: "answer\n\n", want: "answer"},
		{name: "interior newlines kept", write: true, content: "a\nb\n", want: "a\nb"},
		{name: "no trailing newline", write: true, content: "answer", want: "answer"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One path per case so the missing-file case cannot read a sibling's file.
			p := filepath.Join(dir, "out"+strconv.Itoa(i))
			if tc.write {
				if err := os.WriteFile(p, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readFile(p)
			if err != nil {
				t.Fatalf("readFile: %v", err)
			}
			if got != tc.want {
				t.Fatalf("readFile = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestReadFileCapsAtMaxReadBytes: pre-sizing the read from the file's own size
// must not widen the cap that keeps a runaway agy from OOMing the server, which
// is the one place the Stat-derived size and the LimitReader interact. The
// fixture is sparse (Truncate with no writes), so it costs a file size rather
// than 32 MiB of disk traffic; the bytes it reads back are NULs, which
// TrimRight leaves alone, so the length is the whole assertion.
func TestReadFileCapsAtMaxReadBytes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxReadBytes + 1024); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(p)
	if err != nil {
		t.Fatalf("readFile: %v", err)
	}
	if len(got) != maxReadBytes {
		t.Fatalf("read %d bytes, want the %d-byte cap", len(got), maxReadBytes)
	}
}
