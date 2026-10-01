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

var agyVersionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "agy", agyVersionRE)
}

// settingsPath is agy's own settings file, which terma only reads; the settings.json one
// level up belongs to another tool.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), nil
}

// trustsWorkspace reports whether root is in agy's `trustedWorkspaces`; a missing file or
// list is a fresh machine, not an error.
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
	// agy records the path opened: a repository reached through a symlink is still trusted.
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
