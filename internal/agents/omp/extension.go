package omp

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// OMP exports its own telemetry, so terma's extension is hooks only: it hands sessions,
// prompts, file edits and a subagent's parent to `terma hook omp-*`, and asks OMP for full
// content where every signal goes to the relay, whose address it holds, never its token.

//go:embed terma-omp.ts
var extensionTemplate string

const extensionConfigMarker = "const CONFIG: TermaConfig | null = null /* terma:config */"

type extensionConfig struct {
	Version     int      `json:"version"`
	HookCommand []string `json:"hookCommand"`
	Relay       string   `json:"relay"`
}

func renderExtension(hookCommand []string, relay string) (string, error) {
	cfg := extensionConfig{Version: 1, HookCommand: hookCommand, Relay: strings.TrimRight(relay, "/")}
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(extensionTemplate, extensionConfigMarker) {
		return "", errors.New("omp extension template has no configuration line")
	}
	return strings.Replace(extensionTemplate, extensionConfigMarker, "const CONFIG: TermaConfig | null = "+string(data)+" /* terma:config */", 1), nil
}

// writeExtension renders the extension to path, private to this user like the rest of
// OMP's agent directory.
func writeExtension(path string, hookCommand []string, relay string) (string, error) {
	text, err := renderExtension(hookCommand, relay)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, config.WriteFileAtomic(path, []byte(text), 0o600)
}

// extensionCurrent reports whether path holds the extension this terma renders, with a
// configuration naming a hook command, whichever, and the relay at addr (host:port).
func extensionCurrent(path, addr string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	head, rest, ok := strings.Cut(string(data), "const CONFIG: TermaConfig | null = ")
	if !ok {
		return false
	}
	literal, tail, ok := strings.Cut(rest, " /* terma:config */")
	var cfg extensionConfig
	// A configuration that is no rendered one (the template's null) leaves the extension inert.
	if !ok || json.Unmarshal([]byte(literal), &cfg) != nil || cfg.Version != 1 || cfg.Relay != "http://"+addr || len(cfg.HookCommand) == 0 || strings.TrimSpace(cfg.HookCommand[0]) == "" {
		return false
	}
	return head+extensionConfigMarker+tail == extensionTemplate
}
