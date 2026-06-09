package manager

import (
	"context"
	"os/exec"
	"strings"
)

// ListModels runs `agy models` and returns the available model names.
func (m *Manager) ListModels(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, m.cfg.AgyPath, "models").Output()
	if err != nil {
		return nil, err
	}
	return parseModels(string(out)), nil
}

func parseModels(raw string) []string {
	var models []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		models = append(models, line)
	}
	return models
}
