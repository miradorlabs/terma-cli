package shim

import (
	"fmt"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/harness"
	termaproject "github.com/miradorlabs/terma-cli/internal/project"
)

// ClaudeDir is the per-project directory holding the settings document and the headers
// helper a routed Claude Code session is started with.
func ClaudeDir(projectID string) (string, error) {
	if !termaproject.ValidID(projectID) {
		return "", fmt.Errorf("unsafe project id %q", projectID)
	}
	return dir("claude", projectID)
}

// claudeRoutePaths names the two files of a project's Claude route. The helper is kept
// here rather than in harness.HelpersDir so that a machine-wide `terma disconnect
// claude` for the same project, which deletes the helper it installed, cannot take the
// per-repository route down with it.
func claudeRoutePaths(projectID string) (settings, helper string, err error) {
	d, err := ClaudeDir(projectID)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(d, "settings.json"), filepath.Join(d, "otel-headers"), nil
}

// PrepareClaudeSettings writes (or refreshes) the settings document a routed Claude Code
// session is started with, and returns its path. See harness.Claude.WriteRouteSettings
// for why the route travels through `--settings` rather than the environment.
func PrepareClaudeSettings(exp harness.Exporter) (string, error) {
	settings, helper, err := claudeRoutePaths(exp.ProjectID)
	if err != nil {
		return "", err
	}
	if err := (harness.Claude{}).WriteRouteSettings(settings, helper, exp); err != nil {
		return "", err
	}
	return settings, nil
}

// Refresh every launch: changed keys and routing settings must take effect without
// depending on an existing generated document's age or schema.
func ensureClaudeSettings(rec Record, key string) (string, error) {
	return PrepareClaudeSettings(exporterFor(rec, key))
}
