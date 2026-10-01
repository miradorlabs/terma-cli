// Package pifamily is terma's exporter extension for the agents that share one extension
// API: one template, spliced with the machine's configuration.
package pifamily

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/config"
)

//go:embed terma.ts
var template string

const configMarker = "const CONFIG: TermaConfig | null = null /* terma:config */"

// Config is what the extension is spliced with; without Lifecycle it only claims the
// session at each prompt and leaves start, end and edits to a committed hook file.
type Config struct {
	Version            int               `json:"version"`
	Agent              string            `json:"agent"`
	Lifecycle          bool              `json:"lifecycle"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// ForRelay exports all content to the local relay, which applies each project's policy.
func ForRelay(agent string, lifecycle bool, cfg agents.RelayConfig) Config {
	return Config{Agent: agent, Lifecycle: lifecycle, Endpoint: cfg.Endpoint, Headers: relayexport.Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand}
}

// Render splices cfg into the template.
func Render(cfg Config) (string, error) {
	cfg.Version = 1
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(template, configMarker) {
		return "", errors.New("pi extension template has no configuration line")
	}
	return strings.Replace(template, configMarker, "const CONFIG: TermaConfig | null = "+string(data)+" /* terma:config */", 1), nil
}

// Write renders cfg to path, private to this user since it holds the relay's token.
func Write(path string, cfg Config) (string, error) {
	text, err := Render(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, config.WriteFileAtomic(path, []byte(text), 0o600)
}
