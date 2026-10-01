package harness

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/config"
	"github.com/miradorlabs/terma-cli/internal/project"
)

// A headers helper is a 0700 script an agent runs to fetch its OTLP headers, so its
// settings file carries a path and never the server key.

// HelpersDir is where every helper script lives: ~/.config/terma/helpers.
func HelpersDir() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "helpers"), nil
}

// HelperFilePath names the script for one agent and project, re-validating the id where it
// becomes a path, since a "../" in it would put a live key wherever it pointed.
func HelperFilePath(h Harness, projectID string) (string, error) {
	if !project.ValidID(projectID) {
		return "", fmt.Errorf("invalid team id %q: expected letters, digits, dot, dash or underscore", projectID)
	}
	dir, err := HelpersDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, h.Name()+"-otel-"+projectID), nil
}

var helperKeyRE = serverkey.Pattern

// WriteHelper writes the script 0700 in a 0700 directory: it holds a secret and must be executable.
func WriteHelper(path, key string) error {
	if strings.ContainsAny(key, `'"\$`+"`\n") {
		return errors.New("key contains characters that cannot be embedded in a helper script")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	script := fmt.Sprintf(`#!/bin/sh
# Written by 'terma telemetry connect'. The agent runs this to fetch the
# Authorization header for its OTLP export, so the key never sits in its
# settings file. Managed by 'terma telemetry disconnect'; do not edit.
echo '{"Authorization": "Bearer %s"}'
`, key)
	return config.WriteFileAtomic(path, []byte(script), 0o700)
}

// KeyFromHelper extracts the key from a helper script, tolerating hand edits, or "" when there is none.
func KeyFromHelper(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return helperKeyRE.FindString(string(data))
}

// DeleteHelper removes a headers helper script; one already gone is not an error.
func DeleteHelper(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// IsOwnHelper reports whether a configured headers-helper value points into Terma's
// helpers directory; anyone else's script is a conflict.
func IsOwnHelper(value string) bool {
	dir, err := HelpersDir()
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return strings.HasPrefix(abs, dir+string(filepath.Separator))
}
