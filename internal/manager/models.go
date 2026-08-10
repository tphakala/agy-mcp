package manager

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// listModelsTimeout bounds `agy models`, and listModelsKillGrace bounds how long
// after that deadline the call waits before abandoning its output pipes. Same
// hazard as the version probe: agy's descendants inherit those pipes, so one
// that outlives agy would otherwise hold the call open indefinitely.
const (
	listModelsTimeout   = 30 * time.Second
	listModelsKillGrace = time.Second
)

// Model is one entry from `agy models`: the id agy accepts as --model, and the
// display label it prints beside it.
//
// The two are not interchangeable, which is why this is a pair rather than one
// string. Measured against agy 1.1.11: agy takes either an id or a display
// label as --model on its own, but rejects a whole row ("<id>\t<label>") as an
// unrecognized model. Once --effort is set too, every display label in the 1.1.11
// catalog was refused at every effort, while some ids are accepted alongside
// one. That is why list_models hands callers the id (issue #135).
//
// Which efforts a given id accepts is agy's own business and deliberately not
// restated here: it varies per model and some ids accept none at all. Two
// earlier versions of this comment generalized a rule from a handful of probes
// and were wrong both times, so state only what a probe covered.
type Model struct {
	ID    string
	Label string
}

// modelID reduces a model value to the id agy accepts.
//
// `agy models` prints "<id>\t<label>" per row, so a caller that copies a whole
// row (or an operator who sets AGY_MCP_DEFAULT_MODEL to one) supplies a string
// agy rejects outright ("is not recognized as a known model", measured against
// agy 1.1.11). Keeping the part before the first tab turns it back into the id.
//
// A value that is not a whole row is forwarded rather than guessed at: agy owns
// the model namespace, and a bare display label in particular is one agy accepts
// on its own.
//
// It never turns a non-empty value into an empty one, which is why the empty-id
// case returns v rather than "". An empty model makes buildAgyArgs omit --model
// altogether, so the run would silently use agy's own default; forwarding the
// value instead lets agy refuse it, which is the loud failure a caller can act
// on. The trimming matches parseModels, so a row and a hand-typed id reduce to
// the same value (issue #135).
func modelID(v string) string {
	id, _, _ := strings.Cut(strings.TrimSpace(v), "\t")
	if id = strings.TrimSpace(id); id == "" {
		return v
	}
	return id
}

// ListModels runs `agy models` and returns the available models.
func (m *Manager) ListModels(ctx context.Context) ([]Model, error) {
	// Version-gated like the job path even though `agy models` itself does not
	// need stream-json: an agy too old to drive is a configuration problem, and
	// one clear message about it beats a model list from a binary that cannot
	// actually run a job.
	agy, err := m.agyBinaryChecked(ctx)
	if err != nil {
		return nil, err
	}
	// Bounded and WaitDelay'd for the same reason as the version probe: Output()
	// collects through pipes that agy's descendants inherit, so one that outlives
	// agy holds the read open and parks this call for as long as the client keeps
	// it. The caller's ctx alone is not a bound, since it is the raw request
	// context and a client may hold that open indefinitely.
	ctx, cancel := context.WithTimeout(ctx, listModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, agy, "models")
	cmd.WaitDelay = listModelsKillGrace
	out, err := cmd.Output()
	if err != nil {
		// Output() captures stderr into (*exec.ExitError).Stderr; include it so a
		// real cause (an auth prompt, a usage error) is visible instead of a bare
		// "exit status 1".
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("agy models: %w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("agy models: %w", err)
	}
	return parseModels(string(out)), nil
}

// parseModels turns `agy models` output into one Model per non-blank line,
// splitting each row on its first tab.
//
// Splitting on the FIRST tab keeps a label that itself contains a tab attached
// to the label rather than truncating the row. A line with no tab yields an id
// with an empty label instead of being dropped, so output that carries no label
// column still produces usable model values.
func parseModels(raw string) []Model {
	var models []Model
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		id, label, _ := strings.Cut(line, "\t")
		models = append(models, Model{ID: strings.TrimSpace(id), Label: strings.TrimSpace(label)})
	}
	return models
}
