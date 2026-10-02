package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents"
	"github.com/miradorlabs/terma-cli/internal/harness"
	"github.com/miradorlabs/terma-cli/internal/routing"
	"github.com/miradorlabs/terma-cli/internal/ui/output"
)

// Exporter is the export the plan asks for: endpoint and signals as given. It names no
// content: agents send all of it to the relay, and the team's policy decides what leaves.
func (Plan) Exporter(endpoint string, signals []harness.Signal) harness.Exporter {
	return harness.Exporter{Endpoint: endpoint, Signals: signals}
}

// RouteRecord is the routing record for the selected agents' relay targets; ok is false
// when none of them routes through the relay.
func RouteRecord(reg *agents.Registry, projectID string, selected []string, want harness.Exporter) (rec routing.Record, ok bool) {
	targets := reg.RelayTargets(selected)
	if len(targets) == 0 {
		return routing.Record{}, false
	}
	signals := make([]string, 0, len(want.Signals))
	for _, s := range want.Signals {
		signals = append(signals, string(s))
	}
	return routing.Record{ProjectID: projectID, Endpoint: want.Endpoint, Signals: signals,
		Harnesses: targets, Surfaces: RoutedSurfaces(reg, selected, targets)}, true
}

// WriteRepoPolicy writes want as each exporter's repository policy, keeping an existing
// one unless update, and returns the files it changed relative to root. A policy that
// still withholds content, as an earlier terma's could, is rewritten with its own
// signals: content is the team policy's alone. A conflict is skipped rather than
// failing: the hooks and binding are already written, and failing would leave the
// repository half-onboarded.
func WriteRepoPolicy(r Reporter, root string, hs []harness.Harness, want harness.Exporter, update bool) ([]string, error) {
	var written []string
	for _, h := range hs {
		status, err := h.Status()
		if err != nil {
			return nil, err
		}
		want := want
		if status.HasPolicy && !update {
			if !status.StaleContent {
				continue
			}
			want.Signals = status.Signals
		}
		// want.Endpoint is carried, never written: an outranking per-signal redirect is judged against it.
		conflicts, err := h.ConflictsWith(want)
		if err != nil {
			return nil, err
		}
		if conflicts, _ = harness.Partition(conflicts); len(conflicts) > 0 {
			keys := make([]string, 0, len(conflicts))
			for _, c := range conflicts {
				keys = append(keys, c.Key)
			}
			r.Warn("Repo policy", fmt.Sprintf("%s skipped — this repository already has OTLP settings for it (%s)",
				h.DisplayName(), output.SanitizeTerminal(strings.Join(keys, ", "))))
			r.Then(fmt.Sprintf("Resolve %s's OTLP settings in this repository, then run `terma install` here again.", h.DisplayName()))
			continue
		}
		path, err := h.ConfigPath()
		if err != nil {
			return nil, err
		}
		before, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := h.Connect(want, false); err != nil {
			return nil, fmt.Errorf("write %s repository policy: %w", h.Name(), err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(before, after) {
			written = append(written, rel)
		}
		fmt.Fprintf(r.Detail(), "\nWrote %s's repository policy to %s.\n", h.DisplayName(), path)
		r.OK("Repo policy", h.DisplayName()+" telemetry settings in "+rel)
	}
	return written, nil
}
