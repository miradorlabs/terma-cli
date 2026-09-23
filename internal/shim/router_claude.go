package shim

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/keystore"
)

// AgentClaude is Claude Code's binary name and its key in the keystore and the record.
const AgentClaude = "claude"

// claudeRouter routes Claude Code through a per-project `--settings` document. Claude runs
// in the shell's cwd, so its working directory needs no resolution.
type claudeRouter struct{}

func (claudeRouter) name() string { return AgentClaude }

func (claudeRouter) workingDir(cwd string, _ []string) string { return cwd }

func (claudeRouter) routeArgs(rec Record, userArgs []string) []string {
	key := keystore.GetFor(AgentClaude, rec.ProjectID)
	if key == "" {
		return nil
	}
	// Explicit user settings take precedence, and on a preparation failure we pass
	// through rather than half-configure telemetry through an unreliable environment
	// fallback.
	if hasSettingsFlag(userArgs) {
		return nil
	}
	settings, err := ensureClaudeSettings(rec, key)
	if err != nil {
		return nil
	}
	return []string{"--settings", settings}
}

// hasSettingsFlag reports whether the agent's own arguments already carry --settings.
func hasSettingsFlag(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "--settings" || strings.HasPrefix(a, "--settings=") {
			return true
		}
	}
	return false
}
