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

// Pi (@earendil-works/pi-coding-agent) has no OpenTelemetry of its own. terma's
// extension (pi/terma.ts, written into Pi's agent directory) is its exporter and calls
// `terma hook pi-*` for its session lifecycle and file edits. For now it is written by
// `terma relay setup --harness pi` only, pointed at the local relay, which decides per
// session and project what leaves the machine.

//go:embed pi/terma.ts
var piExtensionTemplate string

// piConfigMarker is the line of the template the machine's configuration replaces.
const piConfigMarker = "const CONFIG: TermaConfig | null = null /* terma:config */"

// PiConfig is what the extension is spliced with.
type PiConfig struct {
	Version            int               `json:"version"`
	Endpoint           string            `json:"endpoint"`
	Headers            map[string]string `json:"headers"`
	IncludePrompts     bool              `json:"includePrompts"`
	IncludeToolContent bool              `json:"includeToolContent"`
	HookCommand        []string          `json:"hookCommand"`
}

// PiAgentDir is Pi's user configuration directory: PI_CODING_AGENT_DIR, else ~/.pi/agent.
func PiAgentDir() (string, error) {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// PiExtensionPath is where terma's extension lives.
func PiExtensionPath() (string, error) {
	dir, err := PiAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "extensions", "terma.ts"), nil
}

// RenderPiExtension splices cfg into the extension template.
func RenderPiExtension(cfg PiConfig) (string, error) {
	cfg.Version = 1
	if len(cfg.HookCommand) == 0 {
		cfg.HookCommand = []string{"terma", "hook"}
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if !strings.Contains(piExtensionTemplate, piConfigMarker) {
		return "", errors.New("pi extension template has no configuration line")
	}
	return strings.Replace(piExtensionTemplate, piConfigMarker, "const CONFIG: TermaConfig | null = "+string(data)+" /* terma:config */", 1), nil
}

// WritePiExtension writes terma's extension, configured by cfg, into Pi's agent
// directory, and returns its path. The file holds the relay's local token, so it is
// private to this user.
func WritePiExtension(cfg PiConfig) (string, error) {
	path, err := PiExtensionPath()
	if err != nil {
		return "", err
	}
	text, err := RenderPiExtension(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, config.WriteFileAtomic(path, []byte(text), 0o600)
}
