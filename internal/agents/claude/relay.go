package claude

import (
	"context"
	"net"
	"net/url"
	"path/filepath"
	"runtime"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/agents/internal/relayexport"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// relayHelper is whether the relay's token reaches Claude Code through a headers helper
// (otelHeadersHelper, which Connect writes for any key) rather than the settings' env block, whose
// OTEL_* entries Claude Code releases before 2.1.128 passed to every tool they ran. Not on
// Windows, where the helper is a sh script Claude Code cannot start directly, and a failing
// helper stops the export altogether: there the token stays in the env block.
var relayHelper = runtime.GOOS != "windows"

// relayHelperPath is the relay's headers helper under terma's config directory dir, named
// apart from the per-team helpers (<agent>-otel-<team>).
func relayHelperPath(dir string) string {
	return filepath.Join(harness.HelpersDir(dir), name+"-relay")
}

// ConfigureRelay points Claude Code's own exporter at the local relay.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return agents.RelayResult{}, exporter{dir: a.ConfigDir}.connectRelay(cfg.Endpoint, cfg.Token, true)
}

// connectRelay connects the exporter to the relay at endpoint with all content, the
// relay withholding it per the team's policy.
func (c exporter) connectRelay(endpoint, token string, clearConflicts bool) error {
	e := harness.Exporter{Endpoint: endpoint, APIKey: token, Signals: harness.AllSignals}
	if relayHelper {
		e.HelperPath = relayHelperPath(c.dir)
	}
	return c.Connect(e, clearConflicts)
}

// moveRelayToken moves the relay's token an earlier terma wrote into the settings' env block
// into the relay's headers helper. Only an export still exactly as terma wrote it, with no
// headers helper of the developer's, moves; anything else, an unreadable record included,
// is left for the next setup.
func (c exporter) moveRelayToken() (bool, error) {
	if !relayHelper || c.root != "" {
		return false, nil
	}
	path, err := c.ConfigPath()
	if err != nil {
		return false, err
	}
	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil || j == nil {
		return false, nil
	}
	s, err := loadSettings(path)
	if err != nil || stringSetting(s.root, claudeOtelHeadersHelper) != "" {
		return false, nil
	}
	endpoint := s.env[harness.EnvOTLPEndpoint]
	token := keyFromHeaders(j.Installed[harness.EnvOTLPHeaders])
	if token == "" || serverkey.Is(token) || !loopbackHTTP(endpoint) {
		return false, nil
	}
	for key, installed := range j.Installed {
		if value, ok := s.env[key]; !ok || value != installed {
			return false, nil
		}
	}
	return true, c.connectRelay(endpoint, token, false)
}

// loopbackHTTP is whether endpoint is plain HTTP to this machine, where the relay listens.
func loopbackHTTP(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()
}

// UnconfigureRelay restores Claude Code's own exporter settings to what they were before setup.
func (a Agent) UnconfigureRelay(context.Context, string) (agents.RelayResult, error) {
	return relayexport.Unnative(exporter{dir: a.ConfigDir})
}

// RelayPointed reports whether Claude Code's exporter sends to the relay at addr.
func (a Agent) RelayPointed(addr string) (bool, bool) {
	return relayexport.NativePointed(exporter{dir: a.ConfigDir}, addr), true
}

var _ agents.RelayExporter = Agent{}

// Tool is the label Claude Code's hooks and trailers carry.
func (Agent) Tool() string { return "claude-code" }

var _ agents.Labeled = Agent{}
