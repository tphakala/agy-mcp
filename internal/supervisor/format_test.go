package supervisor

import "testing"

// selectsStreamJSON gates the marker the manager reads to tell a run this build
// drove from a job an older one wrote, so a wrong answer either claims a stream
// that was never produced or loses the marker for one that was. It is untagged
// production code, so this test is untagged too: the only other coverage of the
// marker lives behind a linux||darwin tag.
func TestSelectsStreamJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"separate value", []string{"-p", "hi", "--output-format", "stream-json"}, true},
		{"inline value", []string{"--output-format=stream-json", "-p", "hi"}, true},
		{"another format, separate", []string{"--output-format", "json", "-p", "hi"}, false},
		{"another format, inline", []string{"--output-format=json"}, false},
		{"no format at all", []string{"-p", "hi"}, false},
		// A trailing flag with nothing after it: agy's own parse fails, and this
		// must not read the missing value as a selection.
		{"flag with no value", []string{"-p", "hi", "--output-format"}, false},
		// The Go flag package takes the last occurrence, so these must be read
		// the way agy will read them, not the way they are written.
		{"repeated, stream-json first", []string{"--output-format", "stream-json", "--output-format", "json"}, false},
		{"repeated, stream-json last", []string{"--output-format", "json", "--output-format", "stream-json"}, true},
		{"repeated inline, stream-json first", []string{"--output-format=stream-json", "--output-format=json"}, false},
		{"empty value", []string{"--output-format="}, false},
		{"nil args", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := selectsStreamJSON(tc.args); got != tc.want {
				t.Errorf("selectsStreamJSON(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// The two functions have to agree: whenever ensureStreamJSON appends the flag,
// the result must satisfy selectsStreamJSON, or the supervisor would run a job
// as stream-json and then decline to record that it had.
func TestUpgradedArgsAlwaysSelectStreamJSON(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"-p", "hi"},
		{"--dangerously-skip-permissions", "-p", "hi"},
		{},
		nil,
	} {
		if got := ensureStreamJSON(args); !selectsStreamJSON(got) {
			t.Errorf("ensureStreamJSON(%q) = %q, which does not select stream-json", args, got)
		}
	}
}
