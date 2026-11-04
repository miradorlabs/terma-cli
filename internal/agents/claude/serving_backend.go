package claude

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// servingBackend names who serves the session's calls, as terma.serving.backend's value, from
// the route Claude Code was configured with; "" when it cannot tell. The hook inherits the
// session's environment, which Claude Code's settings env blocks already fed; the settings
// files are read only for what the environment does not say.
func servingBackend(repoRoot string) string {
	configPath, err := (exporter{}).ConfigPath()
	if err != nil {
		return ""
	}
	// Highest precedence first, as Claude Code layers them.
	var paths []string
	if repoRoot != "" {
		paths = append(paths, filepath.Join(repoRoot, ".claude", "settings.local.json"), filepath.Join(repoRoot, ".claude", "settings.json"))
	}
	paths = append(paths, configPath)
	var envs []map[string]json.RawMessage
	helper := false
	for _, path := range paths {
		settings, _ := hookrun.ReadEvidenceJSON(path)
		var env map[string]json.RawMessage
		if json.Unmarshal(settings["env"], &env) == nil {
			envs = append(envs, env)
		}
		var command string
		if json.Unmarshal(settings["apiKeyHelper"], &command) == nil && strings.TrimSpace(command) != "" {
			helper = true
		}
	}
	lookup := func(key string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		for _, env := range envs {
			var v string
			if json.Unmarshal(env[key], &v) == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}
	return routeBackend(lookup, helper || readFunding(repoRoot).Status == hookrun.StatusPresent)
}

// routeBackend maps Claude Code's route to the closed set: a cloud provider's switch, in the
// order Claude Code checks them, else ANTHROPIC_BASE_URL's host, else Anthropic when a
// first-party credential is visible (a key, a token, an apiKeyHelper or the stored login:
// storedCredential covers the last two).
func routeBackend(lookup func(string) string, storedCredential bool) string {
	switch {
	case isOn(lookup("CLAUDE_CODE_USE_BEDROCK")):
		return semconv.TermaServingBackendAwsBedrock
	case isOn(lookup("CLAUDE_CODE_USE_VERTEX")):
		return semconv.TermaServingBackendGoogleVertex
	case isOn(lookup("CLAUDE_CODE_USE_FOUNDRY")):
		return semconv.TermaServingBackendAzureFoundry
	}
	if base := lookup("ANTHROPIC_BASE_URL"); base != "" {
		return baseURLBackend(base)
	}
	if storedCredential || lookup("ANTHROPIC_API_KEY") != "" || lookup("ANTHROPIC_AUTH_TOKEN") != "" {
		return semconv.TermaServingBackendAnthropic
	}
	return ""
}

// baseURLBackend recognises the hosts each backend serves its API from; any other host (a
// gateway, a proxy, a local server) is "", since what stands behind it is unknown.
func baseURLBackend(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "api.anthropic.com":
		return semconv.TermaServingBackendAnthropic
	case (strings.HasPrefix(host, "bedrock-runtime.") || strings.HasPrefix(host, "bedrock-runtime-fips.")) &&
		(strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn")):
		return semconv.TermaServingBackendAwsBedrock
	case host == "aiplatform.googleapis.com" || strings.HasSuffix(host, "-aiplatform.googleapis.com"):
		return semconv.TermaServingBackendGoogleVertex
	case strings.HasSuffix(host, ".services.ai.azure.com"):
		return semconv.TermaServingBackendAzureFoundry
	}
	return ""
}
