package manager

import (
	"context"
	"encoding/json"
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

// splitModelRow splits a "<id>\t<label>" model row into its id and label, the
// reducer modelID applies to a caller-supplied --model value. `agy models` prints
// rows in this shape, so a caller (or AGY_MCP_DEFAULT_MODEL) can paste a whole
// one; this trims the value, cuts on the FIRST tab (so a label that itself
// contains a tab stays whole), and trims each half. A value with no tab yields
// that value as the id and an empty label. Beyond trimming surrounding
// whitespace it neither invents nor drops content; the caller decides what an
// empty id means.
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

// runJSONListing execs `agy --output-format json <sub>` and returns its stdout,
// applying the version gate, a bounded timeout, and the WaitDelay tolerance that
// list_models and list_agents share. Only the subcommand, its timeout pair, and
// (at the caller) the decoder differ between the two, so they run through here
// rather than each carrying its own copy of this delicate ctx-and-error handling.
//
// This is NOT the sharing issue #160 item 7 weighed and rejected: that was about
// folding the VERSION probe together with a listing, and the two genuinely differ
// (readAgyVersion uses CombinedOutput and tolerates a non-zero exit because
// --version's exit status is not a contract, while a listing uses Output and
// treats any non-zero exit as a failure). The two JSON listings share every one
// of those decisions, so nothing delicate is being generalized across a real
// difference here; the version probe stays separate, as that note intends.
func (m *Manager) runJSONListing(ctx context.Context, sub string, timeout, killGrace time.Duration) ([]byte, error) {
	// Version-gated like the job path even though a listing itself does not need
	// stream-json: an agy too old to drive is a configuration problem, and one
	// clear message about it beats a listing from a binary that cannot run a job.
	agy, err := m.agyBinaryChecked(ctx)
	if err != nil {
		return nil, err
	}
	// Bounded and WaitDelay'd for the same reason as the version probe: Output()
	// collects through pipes that agy's descendants inherit, so one that outlives
	// agy holds the read open and parks this call for as long as the client keeps
	// it. The caller's ctx alone is not a bound, since it is the raw request
	// context and a client may hold that open indefinitely.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// --output-format is agy's global print-mode flag, so it must precede the
	// subcommand; agy rejects `<sub> --output-format json` outright (the subcommand
	// flagset does not define it). The listing banner stays on stderr, so stdout is
	// the envelope alone.
	cmd := probeCmd(ctx, agy, outputFormatFlag, jsonOutputFormat, sub)
	cmd.WaitDelay = killGrace
	out, err := cmd.Output()
	if err != nil {
		// Check the deadline BEFORE classifying the exec error, exactly as
		// readAgyVersion does. A ctx-killed process surfaces as *exec.ExitError,
		// indistinguishable from a binary that merely exited non-zero, so without
		// this a timeout this call imposed would surface as "agy <sub>: signal:
		// killed" and blame agy for a deadline it set (issue #160). Name the real
		// cause instead.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				// The caller gave up; say so rather than blaming a binary that was
				// answering fine.
				return nil, fmt.Errorf("agy %s was cancelled: %w", sub, ctxErr)
			}
			return nil, fmt.Errorf("agy %s did not complete within %s: %w", sub, timeout, ctxErr)
		}
		// WaitDelay fired while the deadline had NOT: agy answered and exited, but a
		// descendant kept the output pipe open, so exec abandoned the copy. The
		// envelope it printed is already buffered in out, and it is the whole point
		// of the call. Refusing here would reject a working agy for the exact reason
		// WaitDelay was set, so return the buffer and let the caller's decoder decide;
		// a genuinely truncated envelope then fails at the parse with a message that
		// says so. This mirrors readAgyVersion, which carries the same reasoning for
		// the version probe (issue #161). Unlike --version, a non-zero exit is still
		// a failure here, so every other error (ExitError included) is returned.
		if !errors.Is(err, exec.ErrWaitDelay) {
			// Output() captures stderr into (*exec.ExitError).Stderr; include it so a
			// real cause (an auth prompt, a usage error) is visible instead of a bare
			// "exit status 1".
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				if stderr := strings.TrimSpace(string(ee.Stderr)); stderr != "" {
					return nil, fmt.Errorf("agy %s: %w: %s", sub, err, stderr)
				}
			}
			return nil, fmt.Errorf("agy %s: %w", sub, err)
		}
	}
	return out, nil
}

// ListModels lists agy's available models. It decodes the JSON envelope from
// `agy --output-format json models` (agy 1.1.12+) rather than tab-splitting the
// text rows, so the ids and labels come from a typed field instead of a guessed
// column.
func (m *Manager) ListModels(ctx context.Context) ([]Model, error) {
	out, err := m.runJSONListing(ctx, "models", listModelsTimeout, listModelsKillGrace)
	if err != nil {
		return nil, err
	}
	return decodeModelsEnvelope(out)
}

const (
	// jsonOutputFormat is the --output-format value ListModels passes for the
	// models listing: the single-object envelope, distinct from the stream-json
	// format the job pipeline drives (streamJSONFormat).
	jsonOutputFormat = "json"
	// modelsEnvelopeStatusOK and modelsCommandName are the envelope framing
	// decodeModelsEnvelope requires before it trusts the listing. They are the
	// literals agy prints, mirrored in internal/testutil/fakeagy.go.
	modelsEnvelopeStatusOK = "SUCCESS"
	modelsCommandName      = "models"
)

// modelsEnvelope is the subset of agy's `--output-format json models` output that
// list_models needs: the status/command framing that proves the payload is the
// models listing, and the id/label objects under command.data.models. Fields agy
// also emits (a redundant `response` text column plus usage and timing metadata)
// are ignored.
//
// Models is a POINTER so decodeModelsEnvelope can tell an absent or null array
// (nil, which a `data`/`models` rename or drop produces) from a present-but-empty
// one (a non-nil pointer to an empty slice, a legitimate empty catalog). A plain
// slice collapses both to nil and would read a renamed field as an empty catalog.
type modelsEnvelope struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Models *[]struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"models"`
		} `json:"data"`
	} `json:"command"`
}

// decodeModelsEnvelope turns agy's JSON models envelope into Models, validating
// the framing before it trusts the list. It is an error, not a silently empty
// catalog, when the status is not SUCCESS, the command is not `models`, or the
// command.data.models array is absent or null: each is a shape agy-mcp does not
// recognize (a future field rename or restructure, or an error envelope that
// still exited 0), and cmd.Output() only catches a non-zero exit, so a
// well-formed-but-renamed envelope on exit 0 would otherwise decode to zero
// models and mask the change. An envelope that IS the models command and carries
// an explicit empty array is a legitimately empty catalog and returns an empty,
// non-nil slice.
func decodeModelsEnvelope(raw []byte) ([]Model, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("agy models: parse json envelope: %w", err)
	}
	if env.Status != modelsEnvelopeStatusOK {
		return nil, fmt.Errorf("agy models: envelope status %q, want %q", env.Status, modelsEnvelopeStatusOK)
	}
	if env.Command.Name != modelsCommandName {
		return nil, fmt.Errorf("agy models: envelope command %q, want %q", env.Command.Name, modelsCommandName)
	}
	// nil, not empty: an absent or null command.data.models (a renamed or dropped
	// field), as opposed to a present "models":[] which is a real empty catalog.
	if env.Command.Data.Models == nil {
		return nil, errors.New("agy models: envelope has no command.data.models array")
	}
	rows := *env.Command.Data.Models
	models := make([]Model, 0, len(rows))
	for _, mo := range rows {
		models = append(models, Model{ID: mo.ID, Label: mo.Label})
	}
	return models, nil
}
