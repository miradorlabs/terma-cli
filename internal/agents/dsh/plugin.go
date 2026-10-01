package dsh

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// dsh's own OTLP goes to DeepSeek's collector without usage, so terma's Cordis plugin is
// its exporter, inserted into cordis.patch.yml, the home layer every dsh profile loads.

//go:embed plugin/terma.mjs
var dshPluginTemplate string

const dshConfigMarker = "const CONFIG = null /* terma:config */"

type pluginConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// home is dsh's home: DSH_HOME, else ~/.dsh.
func home() (string, error) {
	if d := os.Getenv("DSH_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".dsh"), nil
}

func renderPlugin(cfg pluginConfig) (string, error) {
	cfg.Version = 1
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(dshPluginTemplate, dshConfigMarker) {
		return "", errors.New("dsh plugin template has no configuration line")
	}
	return strings.Replace(dshPluginTemplate, dshConfigMarker, "const CONFIG = "+string(data)+" /* terma:config */", 1), nil
}

// writePlugin writes terma's plugin and appends its insert to dsh's home patch, a YAML
// list of operations, unless it is already there; every other byte stays.
func writePlugin(cfg pluginConfig) (string, error) {
	home, err := home()
	if err != nil {
		return "", err
	}
	text, err := renderPlugin(cfg)
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, "plugins", "terma-relay.mjs")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	// The plugin holds the relay's local token: private to this user.
	if err := config.WriteFileAtomic(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	patch := filepath.Join(home, "cordis.patch.yml")
	existing, err := os.ReadFile(patch)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if strings.Contains(string(existing), path) {
		return path, nil
	}
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	entry := "- insert:\n    - id: terma\n      name: " + quoted + "\n"
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return path, config.WriteFileAtomic(patch, []byte(out+entry), 0o600)
}
