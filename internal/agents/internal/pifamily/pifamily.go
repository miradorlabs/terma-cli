// Package pifamily is terma's exporter extension for Pi and for omp, Pi's fork, whose
// extension events are the same: one template, spliced with the machine's configuration.
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

// configMarker is the line of the template the machine's configuration replaces.
const configMarker = "const CONFIG: TermaConfig | null = null /* terma:config */"

// Config is what the extension is spliced with. Agent names the agent on every record
// and hook; Lifecycle says whether the extension reports session start, end and file
// edits, or only claims the session at each prompt (omp's committed hook file reports
// the rest).
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

// ForRelay is the configuration that exports to the local relay: all content, since the
// relay applies each project's policy.
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

// Write renders cfg to path. The file holds the relay's local token, so it is private
// to this user.
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
