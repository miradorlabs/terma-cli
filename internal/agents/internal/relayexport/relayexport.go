// Package relayexport is what the agents' relay exporters share.
package relayexport

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Native points an agent's own OTLP exporter at the relay with all content; the relay
// withholds it per the team's policy.
func Native(h harness.Harness, cfg agents.RelayConfig) (agents.RelayResult, error) {
	return agents.RelayResult{}, h.Connect(harness.Exporter{Endpoint: cfg.Endpoint, APIKey: cfg.Token, Signals: harness.AllSignals}, true)
}

// Unnative undoes Native, restoring the agent's own exporter settings from its journal,
// and names any it did not restore because they changed since.
func Unnative(h harness.Harness) (agents.RelayResult, error) {
	result, err := h.Disconnect()
	if err != nil || result.Removed+result.Restored+len(result.Skipped) == 0 {
		return agents.RelayResult{}, err
	}
	path, err := h.ConfigPath()
	if err != nil {
		return agents.RelayResult{}, err
	}
	var out agents.RelayResult
	if result.Removed+result.Restored > 0 {
		out.Paths = []string{path}
	}
	for _, key := range result.Skipped {
		out.Notes = append(out.Notes, fmt.Sprintf("Did not restore %s in %s: it was changed after setup. Check it holds what you want.",
			key, output.TildePath(path)))
	}
	return out, nil
}

// RemoveOwn deletes the file or directory terma wrote as an agent's relay exporter.
func RemoveOwn(path string) (agents.RelayResult, error) {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return agents.RelayResult{}, nil
	}
	if err := os.RemoveAll(path); err != nil {
		return agents.RelayResult{}, err
	}
	return agents.RelayResult{Paths: []string{path}}, nil
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

// Own is embedded by an agent whose relay exporter is terma's own extension, which no
// settings file can say is pointed at the relay.
type Own struct{}

// RelayPointed is unknown: the exporter is terma's own.
func (Own) RelayPointed(string) (bool, bool) { return false, false }
