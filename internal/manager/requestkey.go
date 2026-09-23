package manager

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/tphakala/agy-mcp/v2/internal/jobstore"
)

// requestKey identifies a normalized request for idempotency_key replays. It
// hashes what the caller asked for rather than the agy argv built from it, so a
// change to buildAgyArgs does not make a retry that spans an upgrade look like
// a different request (issue #189).
//
// Two rules keep the key stable. It hashes the conversation id the caller gave,
// not one continue_latest resolved from agy's cache: StartJob rejects
// continue_latest together with conversation_id, so a continue_latest request
// always arrives without one, and the cache can move between an attempt and
// its retry. And every field is omitempty, so a field added later must be one
// whose zero value means the behaviour from before it existed; keys persisted
// before the field then still match. The encoding has its own JSON names, so
// renaming a StartRequest field does not change the key either.
//
// It returns "" if encoding fails, which these field types cannot cause; a job
// stored with an empty key is compared by args, and a retry whose key came out
// empty against a keyed job is refused.
func requestKey(req StartRequest) string {
	dirs := req.Dirs
	if len(dirs) == 0 {
		// An absent dirs list and an empty one are the same request, but encode as
		// null and [] respectively.
		dirs = nil
	}
	convID := req.ConversationID
	if req.ContinueLatest {
		convID = ""
	}
	b, err := json.Marshal(struct {
		Cwd              string        `json:"cwd,omitempty"`
		Prompt           string        `json:"prompt,omitempty"`
		Model            string        `json:"model,omitempty"`
		Effort           string        `json:"effort,omitempty"`
		Mode             string        `json:"mode,omitempty"`
		Agent            string        `json:"agent,omitempty"`
		Sandbox          bool          `json:"sandbox,omitempty"`
		Dirs             []string      `json:"dirs,omitempty"`
		SkipProjectRules bool          `json:"skip_project_rules,omitempty"`
		ConversationID   string        `json:"conversation_id,omitempty"`
		ContinueLatest   bool          `json:"continue_latest,omitempty"`
		JSONSchema       string        `json:"json_schema,omitempty"`
		Timeout          time.Duration `json:"timeout,omitempty"`
	}{
		Cwd:              req.Cwd,
		Prompt:           req.Prompt,
		Model:            req.Model,
		Effort:           req.Effort,
		Mode:             req.Mode,
		Agent:            req.Agent,
		Sandbox:          req.Sandbox,
		Dirs:             dirs,
		SkipProjectRules: req.SkipProjectRules,
		ConversationID:   convID,
		ContinueLatest:   req.ContinueLatest,
		JSONSchema:       req.JSONSchema,
		Timeout:          req.Timeout,
	})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// sameRequest reports whether a persisted job was created by the same normalized
// request. A job that recorded a request key is compared by key.
//
// A job written by a build that predates the key is compared by its agy args. A
// release before the implicit --add-dir <cwd> (issue #188) persisted the args
// this build derives with that dir left out, so those are accepted too, unless
// the retry opted out of project rules (whose args already leave it out). The
// one request this can match wrongly is a job from an unreleased build that had
// the implicit dir but no key, created with project_rules false and retried
// with project rules on. Any other args difference still refuses the retry,
// until garbage collection removes the job.
func sameRequest(meta jobstore.Meta, req StartRequest, args []string) bool {
	if meta.RequestKey != "" {
		return meta.RequestKey == requestKey(req)
	}
	if slices.Equal(meta.Args, args) {
		return true
	}
	if req.SkipProjectRules {
		return false
	}
	before := req
	before.SkipProjectRules = true
	return slices.Equal(meta.Args, buildAgyArgs(before))
}
