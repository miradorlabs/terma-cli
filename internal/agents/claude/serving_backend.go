package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/servinghost"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
)

// claudeCloudSwitches select a cloud provider, whose own endpoint Claude Code uses in place
// of ANTHROPIC_BASE_URL, unless the gateway switch is on beside one.
var claudeCloudSwitches = []string{
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_MANTLE",
	"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
}

// claudeServingBackend is terma.serving.backend for the session's calls: private when Claude
// Code sends them to a base URL whose host is not Anthropic's API (a gateway, a proxy or a
// local server), "" when they go to Anthropic, to a cloud provider, or it cannot tell.
func claudeServingBackend(repoRoot string) string {
	lookup := claudeSettingsLookup(repoRoot)
	if !isOn(lookup("CLAUDE_CODE_USE_GATEWAY")) && slices.ContainsFunc(claudeCloudSwitches, func(k string) bool { return isOn(lookup(k)) }) {
		return ""
	}
	return servinghost.Private(lookup("ANTHROPIC_BASE_URL"))
}

// claudeSettingsLookup returns key's value as the session sees it: the hook's environment,
// which Claude Code builds from its settings, then the settings files' env blocks in Claude
// Code's precedence (managed, the project's local and shared settings, the user's). An empty
// value is unset at every level. Presence in files, not effective settings: --settings is not
// observable.
func claudeSettingsLookup(repoRoot string) func(string) string {
	var paths []string
	paths = append(paths, Agent{}.ManagedHookFiles(string(filepath.Separator))...)
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.local.json"), filepath.Join(repoRoot, ".claude", "settings.json"))
	}
	if configPath, err := (exporter{}).ConfigPath(); err == nil {
		paths = append(paths, configPath)
	}
	var envs []map[string]json.RawMessage
	read := false
	return func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		if !read {
			read = true
			for _, path := range paths {
				settings, status := hookrun.ReadEvidenceJSON(path)
				var env map[string]json.RawMessage
				if status == hookrun.StatusPresent && json.Unmarshal(settings["env"], &env) == nil {
					envs = append(envs, env)
				}
			}
		}
		for _, env := range envs {
			var v string
			if json.Unmarshal(env[key], &v) == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
}
