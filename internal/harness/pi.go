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

// PiConfig is what the extension is spliced with. Agent names the agent on every record
// and hook ("pi", or "omp" for Pi's fork, whose extension events are the same);
// Lifecycle says whether the extension reports session start, end and file edits (omp's
// committed hook file already does) or only claims the session at each prompt.
type PiConfig struct {
	Version            int               `json:"version"`
	Agent              string            `json:"agent"`
	Lifecycle          bool              `json:"lifecycle"`
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
	if cfg.Agent == "" {
		cfg.Agent = "pi"
	}
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
	cfg.Agent, cfg.Lifecycle = "pi", true
	return writePiFamilyExtension(path, cfg)
}

// OmpRelayExtensionPath is where omp loads terma's relay extension from: its user
// extensions directory (a file there loads like one in hooks/pre, verified on 18.3).
// It is a different file from the one `terma connect omp` writes (hooks/pre/terma.ts).
func OmpRelayExtensionPath() (string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "extensions", "terma-relay.ts"), nil
}

// WriteOmpRelayExtension writes the same extension for omp, reporting nothing of the
// session's lifecycle (omp's committed hook file does), and returns its path.
func WriteOmpRelayExtension(cfg PiConfig) (string, error) {
	path, err := OmpRelayExtensionPath()
	if err != nil {
		return "", err
	}
	cfg.Agent, cfg.Lifecycle = "omp", false
	return writePiFamilyExtension(path, cfg)
}

func writePiFamilyExtension(path string, cfg PiConfig) (string, error) {
	text, err := RenderPiExtension(cfg)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, config.WriteFileAtomic(path, []byte(text), 0o600)
}
