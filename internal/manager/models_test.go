package manager

import "testing"

func TestParseModels(t *testing.T) {
	raw := "Gemini 3.5 Flash (Medium)\nGemini 3.1 Pro (High)\n\nClaude Opus 4.6 (Thinking)\n"
	got := parseModels(raw)
	want := []string{"Gemini 3.5 Flash (Medium)", "Gemini 3.1 Pro (High)", "Claude Opus 4.6 (Thinking)"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
