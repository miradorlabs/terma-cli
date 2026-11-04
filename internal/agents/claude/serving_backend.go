package claude

import (
	"net/url"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/servinghost"
	"github.com/miradorlabs/terma-cli/internal/hooks/hookrun"
	"github.com/miradorlabs/terma-cli/internal/semconv"
)

// servingBackend names who serves the session's calls, as terma.serving.backend's value, from
// the route Claude Code was configured with; "" when it cannot tell.
func servingBackend(repoRoot string) string {
	f := readFunding(repoRoot)
	// A settings file that cannot be read may hide a cloud switch, a gateway or a key.
	if f.route.unreadable {
		return ""
	}
	return routeBackend(f.route.lookup, f.route.helper || f.Status == hookrun.StatusPresent)
}

// routeBackend maps Claude Code's route to the closed set: a gateway its switch selects, by
// ANTHROPIC_BASE_URL's host; a cloud provider's switch, by its own base URL's host when one is
// set; else ANTHROPIC_BASE_URL's host; else Anthropic when a first-party credential is visible
// (a key, a token, or storedCredential: an apiKeyHelper or the stored login).
func routeBackend(lookup func(string) string, storedCredential bool) string {
	base := lookup("ANTHROPIC_BASE_URL")
	// The gateway switch outranks every cloud switch, and needs a base URL.
	if isOn(lookup("CLAUDE_CODE_USE_GATEWAY")) && base != "" {
		return baseURLBackend(base)
	}
	if c, ok := cloudRoute(lookup); ok {
		own := ""
		if c.baseURLVar != "" {
			own = lookup(c.baseURLVar)
		}
		if own == "" {
			return c.backend
		}
		// Another provider's host is as foreign to this route as a gateway's.
		if b := baseURLBackend(own); b == "" || b == c.backend {
			return b
		}
		return semconv.TermaServingBackendPrivate
	}
	if base != "" {
		return baseURLBackend(base)
	}
	if storedCredential || firstPartyKey(lookup) {
		return semconv.TermaServingBackendAnthropic
	}
	return ""
}

// baseURLBackend recognises the hosts each backend serves its API from (servinghost); any
// other host (a gateway, a proxy, a local server) is private, and a URL with no host is "".
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
	case servinghost.Bedrock(host):
		return semconv.TermaServingBackendAwsBedrock
	case servinghost.Vertex(host):
		return semconv.TermaServingBackendGoogleVertex
	case servinghost.Azure(host):
		return semconv.TermaServingBackendAzureFoundry
	}
	return semconv.TermaServingBackendPrivate
}
