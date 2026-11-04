package claude

import (
	"net/url"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// servingBackend names who serves the session's calls, as terma.serving.backend's value, from
// the route Claude Code was configured with; "" when it cannot tell.
func servingBackend(repoRoot string) string {
	f := readFunding(repoRoot)
	return routeBackend(f.route.lookup, f.route.helper || f.Status == hookrun.StatusPresent)
}

// cloudBaseURLs are the variables that send a cloud provider's requests to another endpoint,
// such as an LLM gateway.
var cloudBaseURLs = map[string]string{
	semconv.TermaServingBackendAwsBedrock:   "ANTHROPIC_BEDROCK_BASE_URL",
	semconv.TermaServingBackendGoogleVertex: "ANTHROPIC_VERTEX_BASE_URL",
	semconv.TermaServingBackendAzureFoundry: "ANTHROPIC_FOUNDRY_BASE_URL",
}

// routeBackend maps Claude Code's route to the closed set: a cloud provider's switch, by the
// host of that provider's base URL when one is set, else ANTHROPIC_BASE_URL's host, else
// Anthropic when a first-party credential is visible (a key, a token, or storedCredential: an
// apiKeyHelper or the stored login).
func routeBackend(lookup func(string) string, storedCredential bool) string {
	if b := cloudBackend(lookup); b != "" {
		if base := lookup(cloudBaseURLs[b]); base != "" {
			return baseURLBackend(base)
		}
		return b
	}
	if base := lookup("ANTHROPIC_BASE_URL"); base != "" {
		return baseURLBackend(base)
	}
	if storedCredential || firstPartyKey(lookup) {
		return semconv.TermaServingBackendAnthropic
	}
	return ""
}

// baseURLBackend recognises the hosts each backend serves its API from; any other host (a
// gateway, a proxy, a local server) is private, and a URL with no host is "".
func baseURLBackend(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	switch {
	case host == "":
		return ""
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
	return semconv.TermaServingBackendPrivate
}
