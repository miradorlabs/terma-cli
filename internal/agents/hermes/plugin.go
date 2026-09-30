package hermes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/miradorlabs/terma-cli/internal/config"
)

// Hermes (Nous Research) has no OpenTelemetry export terma can use, and its shell hooks
// are user-level only and do not fire in its TUI. terma's plugin (hermes/__init__.py,
// written into $HERMES_HOME/plugins/terma/) is its exporter and calls `terma hook
// hermes-*` for its session lifecycle, prompts and file edits. For now it is written by
// `terma relay setup --harness hermes` only, pointed at the local relay, which decides
// per session and project what leaves the machine.

//go:embed plugin/__init__.py
var hermesPluginTemplate string

//go:embed plugin/plugin.yaml
var hermesPluginManifest string

// hermesConfigMarker is the line of the template the machine's configuration replaces.
const hermesConfigMarker = "CONFIG_JSON = None  # terma:config"

// pluginConfig is what the plugin is spliced with.
type pluginConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// home is Hermes's configuration directory: HERMES_HOME, else ~/.hermes.
func home() (string, error) {
	if d := os.Getenv("HERMES_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermes"), nil
}

// pluginDir is where terma's plugin lives.
func pluginDir() (string, error) {
	home, err := home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "plugins", "terma"), nil
}

// renderPlugin splices cfg into the plugin template. The configuration travels as
// a JSON string literal: Go's quoting of it is a valid Python string literal too.
func renderPlugin(cfg pluginConfig) (string, error) {
	cfg.Version = 1
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(hermesPluginTemplate, hermesConfigMarker) {
		return "", errors.New("hermes plugin template has no configuration line")
	}
	return strings.Replace(hermesPluginTemplate, hermesConfigMarker, "CONFIG_JSON = "+strconv.Quote(string(data))+"  # terma:config", 1), nil
}

// writePlugin writes terma's plugin, configured by cfg, into Hermes's plugin
// directory and returns the directory. The plugin holds the relay's local token, so it
// is private to this user.
func writePlugin(cfg pluginConfig) (string, error) {
	dir, err := pluginDir()
	if err != nil {
		return "", err
	}
	text, err := renderPlugin(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := config.WriteFileAtomic(filepath.Join(dir, "plugin.yaml"), []byte(hermesPluginManifest), 0o600); err != nil {
		return "", err
	}
	return dir, config.WriteFileAtomic(filepath.Join(dir, "__init__.py"), []byte(text), 0o600)
}

// enablePlugin turns the plugin on the way Hermes's own settings do: plugins are
// opt-in (plugins.enabled in config.yaml), and `hermes plugins enable` is the supported
// writer of that file. stdin is closed, so the tool-override question it asks is
// declined: the plugin overrides no tool.
func enablePlugin(ctx context.Context) error {
	bin, err := exec.LookPath("hermes")
	if err != nil {
		return errors.New("hermes is not on PATH: enable the plugin with `hermes plugins enable terma`")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "plugins", "enable", "terma")
	cmd.Stdin = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New("hermes plugins enable terma: " + err.Error() + ": " + strings.TrimSpace(string(out)))
	}
	return nil
}
