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

// splitModelRow applies the single row-split rule that modelID and parseModels
// share, so the two cannot drift apart. `agy models` prints "<id>\t<label>" per
// row; this trims the value, cuts on the FIRST tab (so a label that itself
// contains a tab stays whole), and trims each half. A value with no tab yields
// that value as the id and an empty label. It never invents or drops content;
// the caller decides what an empty id means.
func splitModelRow(row string) (id, label string) {
	id, label, _ = strings.Cut(strings.TrimSpace(row), "\t")
	return strings.TrimSpace(id), strings.TrimSpace(label)
}

// modelID reduces a model value to the id agy accepts.
//
// A caller that copies a whole `agy models` row ("<id>\t<label>"), or an operator
// who sets AGY_MCP_DEFAULT_MODEL to one, supplies a string agy rejects outright
// ("is not recognized as a known model", measured against agy 1.1.11); keeping
// the id column alone turns it back into a value agy accepts.
//
// A value that is not a whole row is forwarded rather than guessed at: agy owns
// the model namespace, and a bare display label in particular is one agy accepts
// on its own. The empty-id case returns v (not "") for the same reason: an empty
// model makes buildAgyArgs omit --model altogether and silently fall back to
// agy's own default, so forwarding the value lets agy refuse it instead, which is
// the loud failure a caller can act on (issue #135).
func modelID(v string) string {
	if id, _ := splitModelRow(v); id != "" {
		return id
	}
	return v
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

// parseModels turns `agy models` output into one Model per non-blank line, using
// splitModelRow to split each row into its id and label columns. A line with no
// tab yields an id with an empty label instead of being dropped, so output that
// carries no label column still produces usable model values.
func parseModels(raw string) []Model {
	// One model per line at most, so strings.Count(raw,"\n")+1 is an upper bound on
	// the result: a cheap capacity hint that never under-allocates. It over-allocates
	// by the blank lines skipped, plus one when raw ends in a trailing newline or is
	// empty. Like the list_models handler, it returns a non-nil empty slice for empty
	// input; no caller marshals this value, so nil vs empty is not observable downstream.
	models := make([]Model, 0, strings.Count(raw, "\n")+1)
	for line := range strings.Lines(raw) {
		// splitModelRow trims before it cuts, so a blank or whitespace-only line is
		// the only input that yields an empty id here; a real row never does. That
		// makes the empty id the blank-line skip, with no second TrimSpace.
		id, label := splitModelRow(line)
		if id == "" {
			continue
		}
		models = append(models, Model{ID: id, Label: label})
	}
	return models
}
