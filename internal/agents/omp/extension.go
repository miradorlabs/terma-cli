package omp

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
)

// OMP exports its own telemetry, so terma's extension is hooks only: it hands sessions,
// prompts, file edits and a subagent's parent to `terma hook omp-*`, and asks OMP for full
// content where every signal goes to the relay, whose address it holds, never its token.

//go:embed terma-omp.ts
var extensionTemplate string

const (
	extensionConfigPrefix = "const CONFIG: TermaConfig | null = "
	extensionConfigSuffix = " /* terma:config */"
	extensionConfigMarker = extensionConfigPrefix + "null" + extensionConfigSuffix
)

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
	return strings.Replace(extensionTemplate, extensionConfigMarker, extensionConfigPrefix+string(data)+extensionConfigSuffix, 1), nil
}

// writeExtension renders the extension to path, private to this user like the rest of
// OMP's agent directory.
func writeExtension(path string, hookCommand []string, relay string) (string, error) {
	text, err := renderExtension(hookCommand, relay)
	if err != nil {
		return "", err
	}
	return path, relayexport.WriteOwn(path, []byte(text), 0o600)
}

func parseExtensionConfig(data []byte) (cfg extensionConfig, head, tail string, ok bool) {
	var rest, literal string
	if head, rest, ok = strings.Cut(string(data), extensionConfigPrefix); !ok {
		return extensionConfig{}, "", "", false
	}
	if literal, tail, ok = strings.Cut(rest, extensionConfigSuffix); !ok {
		return extensionConfig{}, "", "", false
	}
	// A configuration that is no rendered one (the template's null) leaves the extension inert.
	if json.Unmarshal([]byte(literal), &cfg) != nil || cfg.Version != 1 || cfg.Relay == "" || len(cfg.HookCommand) == 0 || strings.TrimSpace(cfg.HookCommand[0]) == "" {
		return extensionConfig{}, "", "", false
	}
	return cfg, head, tail, true
}

// extensionCurrent reports whether path holds the extension this terma renders, with a
// configuration naming a hook command, whichever, and the relay at addr (host:port).
func extensionCurrent(path, addr string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	cfg, head, tail, ok := parseExtensionConfig(data)
	return ok && cfg.Relay == "http://"+addr && head+extensionConfigMarker+tail == extensionTemplate
}

// refreshExtension re-splices an installed OMP extension's configuration into this build's template.
func refreshExtension() (string, bool, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", false, err
	}
	return relayexport.RefreshTemplate(filepath.Join(dir, extensionPath), func(data []byte) ([]byte, bool, error) {
		cfg, _, _, ok := parseExtensionConfig(data)
		if !ok {
			return nil, false, nil
		}
		text, err := renderExtension(cfg.HookCommand, cfg.Relay)
		return []byte(text), err == nil, err
	})
}
