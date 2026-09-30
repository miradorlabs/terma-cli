package harness

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// DeepSeek Harness (dsh) sends its own OTLP to DeepSeek's collector, without usage.
// terma's Cordis plugin (dsh/terma.mjs) is its exporter and calls `terma hook dsh-*`;
// `terma relay setup --harness dsh` writes it into $DSH_HOME/plugins and inserts it by
// absolute path into $DSH_HOME/cordis.patch.yml, the home layer every dsh profile loads.

//go:embed dsh/terma.mjs
var dshPluginTemplate string

const dshConfigMarker = "const CONFIG = null /* terma:config */"

// DshConfig is what the plugin is spliced with.
type DshConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// DshHome is dsh's home: DSH_HOME, else ~/.dsh.
func DshHome() (string, error) {
	if d := os.Getenv("DSH_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".dsh"), nil
}

// RenderDshPlugin splices cfg into the plugin template.
func RenderDshPlugin(cfg DshConfig) (string, error) {
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

// WriteDshPlugin writes terma's plugin and makes sure dsh's home patch inserts it,
// returning the plugin's path. The patch file is a YAML list of operations, so one
// more entry is appended — every other byte stays — unless the plugin is already there.
func WriteDshPlugin(cfg DshConfig) (string, error) {
	home, err := DshHome()
	if err != nil {
		return "", err
	}
	text, err := RenderDshPlugin(cfg)
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
