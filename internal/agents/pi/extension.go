package pi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/procinfo"
)

// Pi has no OpenTelemetry of its own, so terma's extension is its exporter, spliced with
// the machine's configuration and written into Pi's agent directory.

//go:embed terma.ts
var extensionTemplate string

const (
	extensionConfigPrefix = "const CONFIG: TermaConfig | null = "
	extensionConfigSuffix = " /* terma:config */"
	extensionConfigMarker = extensionConfigPrefix + "null" + extensionConfigSuffix
)

type extensionConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// relayExtension exports all content to the local relay, which applies each project's policy.
func relayExtension(cfg agents.RelayConfig) extensionConfig {
	return extensionConfig{Endpoint: cfg.Endpoint, Headers: relayexport.Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand}
}

func renderExtension(cfg extensionConfig) (string, error) {
	cfg.Version = 1
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(extensionTemplate, extensionConfigMarker) {
		return "", errors.New("pi extension template has no configuration line")
	}
	return strings.Replace(extensionTemplate, extensionConfigMarker, extensionConfigPrefix+string(data)+extensionConfigSuffix, 1), nil
}

// writeExtension renders cfg to path, private to this user since it holds the relay's token.
func writeExtension(path string, cfg extensionConfig) (string, error) {
	text, err := renderExtension(cfg)
	if err != nil {
		return "", err
	}
	return path, relayexport.WriteOwn(path, []byte(text), 0o600)
}

// readExtensionConfig is the configuration an extension file was written with: the JSON
// on its config line, version 1 (the config's shape, the same at every wire version).
func readExtensionConfig(data []byte) (*extensionConfig, bool) {
	_, rest, ok := strings.Cut(string(data), extensionConfigPrefix)
	if !ok {
		return nil, false
	}
	literal, _, ok := strings.Cut(rest, extensionConfigSuffix)
	var cfg extensionConfig
	if !ok || json.Unmarshal([]byte(literal), &cfg) != nil || cfg.Version != 1 || cfg.Endpoint == "" {
		return nil, false
	}
	return &cfg, true
}

// refreshExtension re-splices an installed Pi extension's configuration into this build's
// template, an earlier wire version's included, and puts this terma in place of a hook
// command that names a bare terma or one that is gone.
func refreshExtension() (string, bool, error) {
	path, err := extensionFile()
	if err != nil {
		return "", false, err
	}
	return relayexport.RefreshTemplate(path, func(data []byte) ([]byte, bool, error) {
		cfg, ok := readExtensionConfig(data)
		if !ok {
			return nil, false, nil
		}
		if len(cfg.HookCommand) == 0 || !filepath.IsAbs(cfg.HookCommand[0]) || !executable(cfg.HookCommand[0]) {
			if terma, err := procinfo.AbsExecutable(); err == nil {
				cfg.HookCommand = []string{terma, "hook"}
			}
		}
		text, err := renderExtension(*cfg)
		return []byte(text), err == nil, err
	})
}
