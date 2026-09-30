package harness

import (
	"path/filepath"
	"runtime"
)

// CodexUserHooksPath is the user hook file shared by Codex's CLI and Desktop.
func CodexUserHooksPath() (string, error) {
	path, err := (Codex{}).ConfigPath()
	return filepath.Join(filepath.Dir(path), "hooks.json"), err
}

// ClaudeManagedHookFiles locates Claude's administrator-deployed settings.
func ClaudeManagedHookFiles(root string) []string {
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join(root, "Library", "Application Support", "ClaudeCode", "managed-settings.json")}
	}
	return []string{filepath.Join(root, "etc", "claude-code", "managed-settings.json")}
}

// CodexManagedHookFiles locates Codex's administrator-deployed requirements.
func CodexManagedHookFiles(root string) []string {
	return []string{filepath.Join(root, "etc", "codex", "requirements.toml")}
}
