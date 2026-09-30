// Package relayexport is what the agents' relay exporters share.
package relayexport

import (
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Native points an agent's own OTLP exporter at the relay, sending everything: the
// relay withholds content per project.
func Native(h harness.Harness, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return agents.RelayResult{}, h.Connect(harness.Exporter{
		Endpoint: cfg.Endpoint, APIKey: cfg.Token, Signals: harness.AllSignals,
		IncludePrompts: true, IncludeToolContent: true,
	}, true)
}

// NativePointed reports whether h's exporter sends to the relay at addr.
func NativePointed(h harness.Harness, addr string) bool {
	st, err := h.Status()
	if err != nil || !st.Connected {
		return false
	}
	ep := strings.TrimRight(st.Endpoint, "/")
	return ep == "http://"+addr || strings.HasPrefix(ep, "http://"+addr+"/")
}

// Headers authenticate an extension's exports to the relay.
func Headers(cfg agents.RelayConfig) map[string]string {
	return map[string]string{"Authorization": "Bearer " + cfg.Token}
}

// PiConfig is the relay configuration of terma's Pi-family extension.
func PiConfig(cfg agents.RelayConfig) harness.PiConfig {
	return harness.PiConfig{Endpoint: cfg.Endpoint, Headers: Headers(cfg),
		IncludePrompts: true, IncludeToolContent: true, HookCommand: cfg.HookCommand}
}
