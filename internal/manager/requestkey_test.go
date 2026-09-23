package manager

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// TestRequestKeyCoversEveryRequestField walks StartRequest by reflection, so a
// field added later that requestKey forgets to hash fails here instead of
// letting a retry that differs only in that field replay the wrong job.
func TestRequestKeyCoversEveryRequestField(t *testing.T) {
	base := StartRequest{Prompt: "review", Cwd: "/repo", Model: "m", Timeout: time.Minute}
	key := requestKey(base)
	if key == "" {
		t.Fatal("requestKey returned empty")
	}
	// IdempotencyKey is the lookup token itself, not part of the run.
	exempt := map[string]bool{"IdempotencyKey": true}
	for f := range reflect.TypeFor[StartRequest]().Fields() {
		r := base
		v := reflect.ValueOf(&r).Elem().FieldByIndex(f.Index)
		switch v.Kind() {
		case reflect.String:
			v.SetString(v.String() + "-changed")
		case reflect.Bool:
			v.SetBool(!v.Bool())
		case reflect.Int64: // time.Duration
			v.SetInt(v.Int() + 1)
		case reflect.Slice:
			v.Set(reflect.ValueOf([]string{"/changed"}))
		default:
			t.Fatalf("field %s has kind %s: teach this test how to change it", f.Name, v.Kind())
		}
		changed := requestKey(r) != key
		if exempt[f.Name] {
			if changed {
				t.Errorf("changing %s changed the key, but it is exempt", f.Name)
			}
			continue
		}
		if !changed {
			t.Errorf("changing %s did not change the key", f.Name)
		}
	}
}

// TestRequestKeyGolden pins the key's encoding. Keys are persisted in meta.json,
// so a change to the encoded names, the omitempty tags or the hash makes stored
// keys stop matching and refuses retries across the upgrade (issue #189).
// If this fails, make the change deliberately and handle the stored keys.
func TestRequestKeyGolden(t *testing.T) {
	req := StartRequest{
		Prompt: "review", Cwd: "/repo", Model: "m", Dirs: []string{"/a"},
		ContinueLatest: true, ConversationID: "resolved", Timeout: time.Minute,
	}
	const want = "a84f1234f41cc2257b0f15fb0d3ecc0d627f912731a098b2ff9b9d2898a468bb"
	if got := requestKey(req); got != want {
		t.Fatalf("requestKey = %s, want %s", got, want)
	}
}

func TestRequestKeyNormalizesEquivalentRequests(t *testing.T) {
	base := StartRequest{Prompt: "review", Cwd: "/repo", Timeout: time.Minute}
	withEmptyDirs := base
	withEmptyDirs.Dirs = []string{}
	if requestKey(withEmptyDirs) != requestKey(base) {
		t.Error("empty and nil dirs produced different keys")
	}
	// continue_latest resolves a conversation from agy's cache, which can move
	// between an attempt and its retry; the key must not follow it.
	first, retry := base, base
	first.ContinueLatest, retry.ContinueLatest = true, true
	first.ConversationID, retry.ConversationID = "", "conv-recorded-by-the-first-run"
	if requestKey(first) != requestKey(retry) {
		t.Error("a continue_latest retry that resolved another conversation got a different key")
	}
}

// TestFindIdempotentJobAcrossBuilds: a keyed job is compared by key, so it
// replays even when this build derives different args. A job without a key
// (written by an older build) is compared by args, accepting the argv a release
// persisted before the implicit --add-dir <cwd> of issue #188, which is the
// retry-across-an-upgrade case of issue #189.
func TestFindIdempotentJobAcrossBuilds(t *testing.T) {
	req := StartRequest{Prompt: "review", Cwd: t.TempDir(), IdempotencyKey: "k", Timeout: time.Minute}
	args := buildAgyArgs(req)
	before := req
	before.SkipProjectRules = true
	preImplicitArgs := buildAgyArgs(before)
	if slices.Equal(args, preImplicitArgs) {
		t.Fatal("test setup: the two builds' args must differ")
	}
	// The argv a v2.8.1 build persisted for this request, spelled out rather than
	// derived, so a later change to buildAgyArgs cannot move both sides at once.
	v281Args := []string{
		"--dangerously-skip-permissions", "--print-timeout", "1m0s", "--output-format", "stream-json",
		"--disable-slash-commands", "-p", "review",
	}
	other := req
	other.Prompt = "something else"
	for _, tc := range []struct {
		name       string
		requestKey string
		args       []string
		wantFound  bool
	}{
		{"keyed job replays despite different args", requestKey(req), preImplicitArgs, true},
		{"keyed job with another request's key is refused", requestKey(other), args, false},
		{"keyless job with this build's args replays", "", args, true},
		{"keyless job from a release before the implicit dir replays", "", preImplicitArgs, true},
		{"keyless job with v2.8.1's literal argv replays", "", v281Args, true},
		{"keyless job for another request is refused", "", buildAgyArgs(other), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newManager(t, managerOpts{})
			if _, err := m.store.Create(jobstore.Meta{
				ID: "j", IdempotencyKey: "k", RequestKey: tc.requestKey, Cwd: req.Cwd, Args: tc.args,
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
