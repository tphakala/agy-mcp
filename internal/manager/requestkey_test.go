package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

func TestRequestKeyDistinguishesRequests(t *testing.T) {
	base := StartRequest{Prompt: "review", Cwd: "/repo", Model: "m", Timeout: time.Minute}
	key := requestKey(base)
	if key == "" {
		t.Fatal("requestKey returned empty")
	}
	// An empty dirs list and an absent one are the same request.
	withEmptyDirs := base
	withEmptyDirs.Dirs = []string{}
	if requestKey(withEmptyDirs) != key {
		t.Error("empty and nil dirs produced different keys")
	}
	// Each row changes one field that defines the run, so each field is pinned.
	for name, mutate := range map[string]func(*StartRequest){
		"prompt":             func(r *StartRequest) { r.Prompt = "other" },
		"cwd":                func(r *StartRequest) { r.Cwd = "/other" },
		"model":              func(r *StartRequest) { r.Model = "m2" },
		"effort":             func(r *StartRequest) { r.Effort = "high" },
		"mode":               func(r *StartRequest) { r.Mode = "plan" },
		"agent":              func(r *StartRequest) { r.Agent = "a" },
		"sandbox":            func(r *StartRequest) { r.Sandbox = true },
		"dirs":               func(r *StartRequest) { r.Dirs = []string{"/x"} },
		"skip project rules": func(r *StartRequest) { r.SkipProjectRules = true },
		"conversation":       func(r *StartRequest) { r.ConversationID = "c" },
		// A continue_latest that resolved no conversation leaves the same args as a
		// fresh run, but the caller asked for something different.
		"continue latest": func(r *StartRequest) { r.ContinueLatest = true },
		"json schema":     func(r *StartRequest) { r.JSONSchema = "{}" },
		"timeout":         func(r *StartRequest) { r.Timeout = time.Hour },
	} {
		r := base
		mutate(&r)
		if requestKey(r) == key {
			t.Errorf("changing %s did not change the key", name)
		}
	}
}

// TestFindIdempotentJobMatchesKeyAcrossArgsChange: a job persisted with a request
// key replays for the same request even when the args this build derives differ
// from the ones persisted, which is the retry-across-an-upgrade case of issue
// #189. A job without a key (written by an older build) is still compared by
// args and fails closed.
func TestFindIdempotentJobMatchesKeyAcrossArgsChange(t *testing.T) {
	req := StartRequest{Prompt: "review", Cwd: t.TempDir(), IdempotencyKey: "k", Timeout: time.Minute}
	args := buildAgyArgs(req)
	olderArgs := buildAgyArgs(StartRequest{Prompt: "review", Cwd: req.Cwd, Timeout: time.Minute, SkipProjectRules: true})
	if strings.Join(args, "\x00") == strings.Join(olderArgs, "\x00") {
		t.Fatal("test setup: the two builds' args must differ")
	}
	for _, tc := range []struct {
		name       string
		requestKey string
		wantFound  bool
	}{
		{"keyed job replays", requestKey(req), true},
		{"keyless job from an older build fails closed", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			if _, err := m.store.Create(jobstore.Meta{
				ID: "j", IdempotencyKey: "k", RequestKey: tc.requestKey, Cwd: req.Cwd, Args: olderArgs,
				Prompt: req.Prompt, PID: 999999, BootID: "old-boot", StartedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			job, found, err := m.findIdempotentJob(req, args)
			if tc.wantFound {
				if err != nil || !found || job.ID != "j" {
					t.Fatalf("findIdempotentJob = %+v, %v, %v; want the existing job", job, found, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "different normalized request") {
				t.Fatalf("findIdempotentJob err = %v, want the different-request refusal", err)
			}
		})
	}
}
