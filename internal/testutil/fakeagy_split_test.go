package testutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// splitChunks must never lose, reorder or corrupt text, because the whole point
// of StreamChunks is that a consumer accumulating every delta reproduces the
// response exactly. Non-ASCII input is included because a byte-wise split would
// cut a multi-byte rune in half and produce invalid UTF-8 that no real agy
// delta ever carries.
//
// This test is deliberately NOT build-tagged, unlike the rest of the fakeagy
// tests: splitChunks is a pure function needing no bash subprocess, so its
// rune-boundary property is worth covering on Windows too, where the fake agy
// script itself cannot run.
func TestSplitChunks(t *testing.T) {
	const text = "the quick brown fox jumps over the lazy dog"
	// A 4-byte rune (U+1D11E, outside the BMP) is included so a byte-wise split has
	// a wide rune to cut through; the earlier fixture's trailing "emoji" was ASCII,
	// so no rune above 3 bytes was ever exercised despite the naming.
	const multibyte = "hyvää yötä äöå 你好世界 \U0001D11E"

	for _, tc := range []struct {
		name      string
		text      string
		n         int
		wantParts int
	}{
		{"zero keeps one piece", text, 0, 1},
		{"one keeps one piece", text, 1, 1},
		{"negative keeps one piece", text, -3, 1},
		{"splits into n", text, 4, 4},
		{"uneven split", text, 5, 5},
		{"n equal to rune count", "abcd", 4, 4},
		// A multi-byte string here, not ASCII: with rune count == byte count the
		// clamp does not discriminate a rune split from a byte split.
		{"n above rune count clamps", "äö", 99, 2},
		{"unicode splits on runes", multibyte, 5, 5},
		{"unicode splits on runes, uneven", multibyte, 7, 7},
		{"empty text", "", 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitChunks(tc.text, tc.n)
			if len(got) != tc.wantParts {
				t.Errorf("len = %d, want %d (parts %q)", len(got), tc.wantParts, got)
			}
			if joined := strings.Join(got, ""); joined != tc.text {
				t.Errorf("concatenation = %q, want the input back %q", joined, tc.text)
			}
			for i, part := range got {
				// An empty piece would silently reduce the delta count a caller asked
				// for, because consumeStream skips an empty text_delta.
				if part == "" && tc.text != "" {
					t.Errorf("piece %d is empty; every piece must carry text", i)
				}
				if !utf8.ValidString(part) {
					t.Errorf("piece %d = %q is not valid UTF-8; the split must land on a rune boundary", i, part)
				}
			}
		})
	}
}
