package harness

import (
	"path/filepath"
)

// CodexUserHooksPath is the user hook file shared by Codex's CLI and Desktop.
func CodexUserHooksPath() (string, error) {
	path, err := (Codex{}).ConfigPath()
	return filepath.Join(filepath.Dir(path), "hooks.json"), err
}

// CodexManagedHookFiles locates Codex's administrator-deployed requirements.
func CodexManagedHookFiles(root string) []string {
	return []string{filepath.Join(root, "etc", "codex", "requirements.toml")}
}
