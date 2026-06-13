package manager

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ListModels runs `agy models` and returns the available model names.
func (m *Manager) ListModels(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, m.cfg.AgyPath, "models").Output()
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
