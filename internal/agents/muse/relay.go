package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/config"
)

// Muse sends its own telemetry wherever settings.json's telemetry.endpoint names, with its
// sign-in as the credential, and turns it off rather than move it for OTEL_EXPORTER_OTLP_ENDPOINT
// (1.4.15). terma points the endpoint at the relay, with the relay's token as the path's first
// segment: the relay routes the claimed sessions' records and passes Meta its copy of every one
// (Mirrors), so Meta receives what it did before. A developer who routes Muse's telemetry
// themselves (an endpoint or destination of their own) or turned it off keeps that: terma sees
// Muse through its hooks alone then.

// relayEndpoint is an endpoint terma wrote: a loopback relay's address and a token.
var relayEndpoint = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost|\[::1\]):[0-9]+/[0-9a-f]{32,}$`)

// ConfigureRelay points Muse's telemetry at the relay, unless the developer routes it.
func (a Agent) ConfigureRelay(_ context.Context, cfg agents.RelayConfig) (agents.RelayResult, error) {
	path, doc, err := a.settings()
	if err != nil {
		return agents.RelayResult{}, err
	}
	tel, _ := doc["telemetry"].(map[string]any)
	if tel == nil {
		tel = map[string]any{}
	}
	if why := ownRouting(tel); why != "" {
		return agents.RelayResult{Notes: []string{"Muse Code: " + why + "; terma leaves it so, and sees Muse through its hooks."}}, nil
	}
	tel["endpoint"] = cfg.Endpoint + "/" + cfg.Token
	doc["telemetry"] = tel
	if _, ok := doc["schema_version"]; !ok {
		doc["schema_version"] = 1
	}
	if err := writeSettings(path, doc); err != nil {
		return agents.RelayResult{}, err
	}
	return agents.RelayResult{Paths: []string{path}, Notes: []string{"Muse Code: its own telemetry goes through the relay, which still sends Meta its copy."}}, nil
}

// ownRouting says how the developer routes Muse's telemetry themselves, "" where Muse sends it
// where it does by default (or to a relay).
func ownRouting(tel map[string]any) string {
	if on, ok := tel["enabled"].(bool); ok && !on {
		return "its telemetry is off (telemetry.enabled)"
	}
	if ep, _ := tel["endpoint"].(string); ep != "" && !relayEndpoint.MatchString(ep) {
		return "its telemetry goes to an endpoint of your own (telemetry.endpoint)"
	}
	if d, _ := tel["destination"].(string); d != "" {
		return "its telemetry goes to a destination of your own (telemetry.destination)"
	}
	return ""
}

// UnconfigureRelay takes the relay's endpoint out of Muse's settings: Muse sends to Meta again.
func (a Agent) UnconfigureRelay(_ context.Context, token string) (agents.RelayResult, error) {
	path, err := a.UserHooksPath()
	if err != nil {
		return agents.RelayResult{}, err
	}
	// Only settings holding the token are terma's to parse: the developer's own may not be JSON.
	data, _ := os.ReadFile(path)
	if token == "" || !bytes.Contains(data, []byte("/"+token)) {
		return agents.RelayResult{}, nil
	}
	_, doc, err := a.settings()
	if err != nil {
		return agents.RelayResult{}, err
	}
	tel, _ := doc["telemetry"].(map[string]any)
	if ep, _ := tel["endpoint"].(string); !strings.HasSuffix(ep, "/"+token) {
		return agents.RelayResult{}, nil
	}
	delete(tel, "endpoint")
	if len(tel) == 0 {
		delete(doc, "telemetry")
	}
	return agents.RelayResult{Paths: []string{path}}, writeSettings(path, doc)
}

// RelayPointed reports whether Muse's telemetry goes to the relay at addr; not known where the
// developer routes it themselves.
func (a Agent) RelayPointed(addr string) (pointed, known bool) {
	_, doc, err := a.settings()
	if err != nil {
		return false, false
	}
	tel, _ := doc["telemetry"].(map[string]any)
	if ownRouting(tel) != "" {
		return false, false
	}
	ep, _ := tel["endpoint"].(string)
	return strings.HasPrefix(ep, "http://"+addr+"/"), true
}

// settings is Muse's user settings, parsed: empty where there are none.
func (a Agent) settings() (string, map[string]any, error) {
	path, err := a.UserHooksPath()
	if err != nil {
		return "", nil, err
	}
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", nil, err
	default:
		if err := json.Unmarshal(data, &doc); err != nil {
			return "", nil, fmt.Errorf("%s does not parse as JSON, so terma leaves it alone: %w", path, err)
		}
	}
	return path, doc, nil
}

// writeSettings writes doc to path, private to the user: the endpoint carries the relay's token.
func writeSettings(path string, doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return config.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

var _ agents.RelayExporter = Agent{}
