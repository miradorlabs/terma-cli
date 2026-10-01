package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// geminiHome is where Gemini keeps .gemini: GEMINI_CLI_HOME, else the home directory.
func geminiHome() (string, error) {
	if d := os.Getenv("GEMINI_CLI_HOME"); d != "" {
		return d, nil
	}
	return os.UserHomeDir()
}

func settingsPath() (string, error) {
	home, err := geminiHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

func extensionDir() (string, error) {
	home, err := geminiHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "extensions", "terma"), nil
}

var geminiHookEvents = []struct{ gemini, terma string }{
	{"SessionStart", "gemini-session-start"},
	{"BeforeAgent", "gemini-prompt"},
	{"AfterTool", "gemini-after-tool"},
	{"SessionEnd", "gemini-session-end"},
}

// connectRelay points Gemini's exporter at endpoint and writes terma's extension. The
// settings file has no headers setting, so the relay's token rides the endpoint path:
// OTEL_EXPORTER_OTLP_HEADERS would reach everything Gemini runs.
func connectRelay(endpoint string, hookCommand []string) (settings, extension string, err error) {
	if settings, err = settingsPath(); err != nil {
		return "", "", err
	}
	doc := map[string]any{}
	if data, err := os.ReadFile(settings); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return "", "", fmt.Errorf("%s does not parse as JSON, so terma leaves it alone: %w", settings, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	tel, _ := doc["telemetry"].(map[string]any)
	if tel == nil {
		tel = map[string]any{}
	}
	// The relay withholds content per project, so the exporter sends it all.
	for k, v := range map[string]any{"enabled": true, "target": "local", "otlpEndpoint": endpoint, "otlpProtocol": "http", "logPrompts": true, "traces": true} {
		tel[k] = v
	}
	doc["telemetry"] = tel
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		return "", "", err
	}
	// The endpoint carries the relay's token: private to this user.
	if err := config.WriteFileAtomic(settings, append(data, '\n'), 0o600); err != nil {
		return "", "", err
	}

	if extension, err = extensionDir(); err != nil {
		return "", "", err
	}
	hooks := map[string]any{}
	command := shellQuoteArgs(hookCommand)
	for _, e := range geminiHookEvents {
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command + " " + e.terma}}}
		if e.gemini == "AfterTool" {
			entry["matcher"] = "*"
		}
		hooks[e.gemini] = []any{entry}
	}
	hooksJSON, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	manifest, _ := json.MarshalIndent(map[string]any{"name": "terma", "version": "1.0.0"}, "", "  ")
	if err := os.MkdirAll(filepath.Join(extension, "hooks"), 0o700); err != nil {
		return "", "", err
	}
	if err := config.WriteFileAtomic(filepath.Join(extension, "gemini-extension.json"), append(manifest, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return settings, extension, config.WriteFileAtomic(filepath.Join(extension, "hooks", "hooks.json"), append(hooksJSON, '\n'), 0o600)
}

// shellQuoteArgs renders args as one POSIX shell command line, as Gemini runs hooks.
func shellQuoteArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
