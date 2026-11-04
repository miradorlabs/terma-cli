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

// routeBackend maps Claude Code's route to the closed set: a cloud provider's switch, by the
// host of that provider's base URL when one is set, else ANTHROPIC_BASE_URL's host, else
// Anthropic when a first-party credential is visible (a key, a token, or storedCredential: an
// apiKeyHelper or the stored login).
func routeBackend(lookup func(string) string, storedCredential bool) string {
	if c, ok := cloudRoute(lookup); ok {
		if base := lookup(c.baseURLVar); base != "" {
			return baseURLBackend(base)
		}
		return c.backend
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
	case strings.HasPrefix(host, "bedrock-mantle.") && strings.HasSuffix(host, ".api.aws"):
		return semconv.TermaServingBackendAwsBedrock
	case host == "aiplatform.googleapis.com" || strings.HasSuffix(host, "-aiplatform.googleapis.com"):
		return semconv.TermaServingBackendGoogleVertex
	case strings.HasSuffix(host, ".services.ai.azure.com"):
		return semconv.TermaServingBackendAzureFoundry
	}
	return semconv.TermaServingBackendPrivate
}
