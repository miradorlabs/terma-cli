package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// agy has no configurable OTLP exporter (its one telemetry switch reports to Google), so
// everything terma learns about an agy session arrives through the repository hooks.
// What is here is the part that has to know agy's file layout: where it is installed,
// and where it records which workspaces the developer trusted, since a workspace's
// hooks.json loads only for a trusted workspace and is silently skipped otherwise.

var agyVersionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

// detect looks for the agy binary and its version.
func detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "agy", agyVersionRE)
}

// settingsPath is agy's own settings file, ~/.gemini/antigravity-cli/settings.json. It is
// agy's, never terma's to write: it holds the trust decisions this package only reads.
// The parent directory is shared with Gemini CLI, whose settings.json one level up is a
// different file with different keys.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), nil
}

// trustsWorkspace reports whether the developer has trusted root as an Antigravity
// workspace. agy records the answer under `trustedWorkspaces` in its settings when the
// developer accepts the trust prompt on opening a folder; a missing file or list means
// nothing has been trusted, which is what a fresh machine looks like and not an error.
func trustsWorkspace(root string) (bool, error) {
	path, err := settingsPath()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var settings struct {
		TrustedWorkspaces []string `json:"trustedWorkspaces"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, err
	}
	// agy records the path the developer opened. Compare the cleaned literal and the
	// resolved one: a repository reached through a symlink must not read as untrusted.
	want := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != want[0] {
		want = append(want, resolved)
	}
	for _, trusted := range settings.TrustedWorkspaces {
		trusted = filepath.Clean(trusted)
		if slices.Contains(want, trusted) {
			return true, nil
		}
		if resolved, err := filepath.EvalSymlinks(trusted); err == nil {
			if slices.Contains(want, resolved) {
				return true, nil
			}
		}
	}
	return false, nil
}
