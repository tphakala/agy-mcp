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

// ListModels runs `agy models` and returns the available model names.
func (m *Manager) ListModels(ctx context.Context) ([]string, error) {
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

func parseModels(raw string) []string {
	var models []string
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		models = append(models, line)
	}
	return models
}
