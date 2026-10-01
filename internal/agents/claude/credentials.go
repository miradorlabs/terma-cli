package claude

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// keyFromHeaders returns the raw key from an OTEL_EXPORTER_OTLP_HEADERS value, for reuse;
// a display must mask it.
func keyFromHeaders(headers string) string {
	for pair := range strings.SplitSeq(headers, ",") {
		name, value, found := strings.Cut(pair, "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "Authorization") {
			continue
		}
		key := strings.TrimSpace(value)
		return strings.TrimSpace(strings.TrimPrefix(key, "Bearer"))
	}
	return ""
}

// CurrentCredential returns the key this config already presents to endpoint for projectID, so a
// reconnect reuses it instead of minting an orphan. Endpoint and project must both match.
func (c exporter) CurrentCredential(endpoint, projectID string) (string, bool) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", false
	}
	s, err := loadSettings(path)
	if err != nil {
		return "", false
	}
	if s.env[harness.EnvOTLPEndpoint] != endpoint {
		return "", false
	}
	j, err := harness.LoadJournal(c.Name(), path)
	if err != nil || projectIDOf(j) != projectID {
		return "", false
	}
	if helper := stringSetting(s.root, claudeOtelHeadersHelper); helper != "" && harness.IsOwnHelper(helper) {
		if key := harness.KeyFromHelper(helper); serverkey.Is(key) {
			return key, true
		}
	}
	if key := keyFromHeaders(s.env[harness.EnvOTLPHeaders]); serverkey.Is(key) {
		return key, true
	}
	return "", false
}

// maskKeyFromHeaders returns only the key's head: status output lands in screenshots.
func maskKeyFromHeaders(headers string) string {
	return harness.MaskKey(keyFromHeaders(headers))
}

func projectIDOf(j *harness.Journal) string {
	if j != nil {
		return j.ProjectID
	}
	return ""
}
