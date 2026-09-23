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
// hashes the request fields rather than the agy argv built from them, so a
// change to buildAgyArgs (such as the implicit --add-dir <cwd> of issue #188)
// does not make a retry that spans an upgrade look like a different request
// (issue #189). The encoding has its own JSON names, so renaming a StartRequest
// field does not change the key either. It returns "" only if encoding fails,
// which sameRequest treats as "no key" and falls back to comparing args.
func requestKey(req StartRequest) string {
	dirs := req.Dirs
	if len(dirs) == 0 {
		// An absent dirs list and an empty one are the same request, but encode as
		// null and [] respectively.
		dirs = nil
	}
	b, err := json.Marshal(struct {
		Cwd              string        `json:"cwd"`
		Prompt           string        `json:"prompt"`
		Model            string        `json:"model"`
		Effort           string        `json:"effort"`
		Mode             string        `json:"mode"`
		Agent            string        `json:"agent"`
		Sandbox          bool          `json:"sandbox"`
		Dirs             []string      `json:"dirs"`
		SkipProjectRules bool          `json:"skip_project_rules"`
		ConversationID   string        `json:"conversation_id"`
		ContinueLatest   bool          `json:"continue_latest"`
		JSONSchema       string        `json:"json_schema"`
		Timeout          time.Duration `json:"timeout"`
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
		ConversationID:   req.ConversationID,
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
// request. A job that recorded a request key is compared by key. A job written by
// a build that predates the key is compared by its agy args, as before, so such
// a job still refuses a retry whose args a newer buildAgyArgs spells differently
// until the job store's retention removes it.
func sameRequest(meta jobstore.Meta, req StartRequest, args []string) bool {
	if meta.RequestKey != "" {
		return meta.RequestKey == requestKey(req)
	}
	return slices.Equal(meta.Args, args)
}
