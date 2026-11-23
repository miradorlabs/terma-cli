package grok

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/agents/internal/tomledit"
	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures grok's native OTLP log export through [telemetry] in grok's
// config.toml, the one place grok reads those keys from (GROK_CONFIG and a project's
// .grok/config.toml cannot carry them). What each key held before is journaled under
// terma's config directory dir, so a disconnect restores it.
type exporter struct {
	dir string
}

const (
	configFileName = "config.toml"
	telemetryTable = "telemetry"

	// The eight keys terma writes: the stream on, logs only, per-signal so a shell's
	// generic OTEL_EXPORTER_OTLP_ENDPOINT or …_PROTOCOL=grpc cannot move or break it, and every
	// content switch on, since the relay withholds per the team's policy.
	keyEnabled               = "otel_enabled"
	keyLogsExporter          = "otel_logs_exporter"
	keyLogsEndpoint          = "otel_logs_endpoint"
	keyLogsProtocol          = "otel_logs_protocol"
	keyLogUserPrompts        = "otel_log_user_prompts"
	keyLogAssistantResponses = "otel_log_assistant_responses"
	keyLogToolDetails        = "otel_log_tool_details"
	keyLogToolContent        = "otel_log_tool_content"

	// Keys terma reads and never writes: a metrics exporter someone switches on must not
	// reach the relay, whose rules do not cover grok's metric points.
	keyEndpoint        = "otel_endpoint"
	keyMetricsExporter = "otel_metrics_exporter"
	keyMetricsEndpoint = "otel_metrics_endpoint"

	exporterOTLP = "otlp"
	logsPath     = "/v1/logs"
)

// managedKeys are the keys a connect writes, in the file's order.
var managedKeys = []string{
	keyEnabled, keyLogsExporter, keyLogsEndpoint, keyLogsProtocol,
	keyLogUserPrompts, keyLogAssistantResponses, keyLogToolDetails, keyLogToolContent,
}

// Name is the token `--harness` accepts.
func (exporter) Name() string { return name }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// Local is unsupported: grok reads [telemetry] from its user config only.
func (exporter) Local(string) (harness.Harness, bool) { return nil, false }

// Detect runs `grok --version`, which prints `grok 1.0.92 (<build>)`; a missing binary is
// not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "grok", harness.SemverRE)
}

// ConfigPath is grok's config.toml in its home directory.
func (exporter) ConfigPath() (string, error) {
	home, err := grokHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, configFileName), nil
}

// relayBase is the relay's endpoint with its token as the first path segment, which grok
// presents in the path: it has no setting for a header.
func relayBase(e harness.Exporter) string {
	base := strings.TrimRight(e.Endpoint, "/")
	if e.APIKey != "" {
		base += "/" + e.APIKey
	}
	return base
}

// managed is what a connect to e writes.
func managed(e harness.Exporter) map[string]any {
	return map[string]any{
		keyEnabled:               true,
		keyLogsExporter:          exporterOTLP,
		keyLogsEndpoint:          relayBase(e) + logsPath,
		keyLogsProtocol:          harness.ProtocolHTTPProtobuf,
		keyLogUserPrompts:        true,
		keyLogAssistantResponses: true,
		keyLogToolDetails:        true,
		keyLogToolContent:        true,
	}
}

// loadConfig reads grok's config.toml for an edit of [telemetry].
func (c exporter) loadConfig() (string, *tomledit.File, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return "", nil, err
	}
	f, err := tomledit.Load(path, telemetryTable)
	return path, f, err
}

// Status reads back where grok's logs go, by value: grok rewrites config.toml in its own
// normalised form when it saves a setting, keeping every key and no comment, so nothing marks
// terma's keys but what they hold. It is connected only while all eight hold what a connect
// to the endpoint named there writes; one missing or changed is drift.
func (c exporter) Status() (harness.Status, error) {
	path, f, err := c.loadConfig()
	if err != nil {
		return harness.Status{}, err
	}
	st := harness.Status{ConfigPath: path, Exists: f.Existed}
	if endpoint, _ := f.Table[keyLogsEndpoint].(string); endpoint != "" {
		if base, ok := strings.CutSuffix(endpoint, logsPath); ok && tokenPath(base) {
			st.Endpoint = base
		}
	}
	st.Connected = st.Endpoint != "" && holds(f.Table, managed(harness.Exporter{Endpoint: st.Endpoint}))
	if st.Connected {
		st.Signals = []harness.Signal{harness.SignalLogs}
	}
	st.Conflicts = grokConflicts(harness.Exporter{Endpoint: st.Endpoint}, f.Table)

	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return harness.Status{}, err
	}
	if j != nil {
		current := rendered(f.Table)
		for key, installed := range j.Installed {
			if v, ok := current[key]; ok && v == installed {
				st.ManagedKeys++
			}
		}
	}
	return st, nil
}

// holds reports whether table holds every value of want.
func holds(table, want map[string]any) bool {
	got, wanted := rendered(table), rendered(want)
	for key, v := range wanted {
		if got[key] != v {
			return false
		}
	}
	return true
}

// rendered is the managed keys table holds, each as canonical TOML text, which stands in for
// the value in the journal; a value that cannot be rendered is left out, as no value of
// terma's.
func rendered(table map[string]any) map[string]string {
	out := map[string]string{}
	for _, key := range managedKeys {
		v, ok := table[key]
		if !ok {
			continue
		}
		if text, err := tomledit.Render(v); err == nil {
			out[key] = text
		}
	}
	return out
}

// ConflictsWith reports shell exports, and keys of grok's own config, that would defeat or
// redirect e.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	_, f, err := c.loadConfig()
	if err != nil {
		return nil, err
	}
	return grokConflicts(e, f.Table), nil
}

// Connect writes the eight keys into [telemetry], leaving every other key and table, and the
// comments outside [telemetry], as they were; the table itself is rendered anew, its keys
// sorted and its own comments gone, as grok's next settings save would leave it. What each
// key held before, or that it was absent, is journaled first, so a failed write leaves nothing
// unrecorded.
func (c exporter) Connect(e harness.Exporter, _ bool) error {
	path, f, err := c.loadConfig()
	if err != nil {
		return err
	}
	previous, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return err
	}
	want := managed(e)
	j := harness.NewJournal(c.Name(), path, rendered(f.Table), rendered(want), nil, nil, previous)
	// The file's mode before terma's first connect, which the connect tightens.
	switch {
	case previous != nil:
		j.ConfigMode = previous.ConfigMode
	case f.Existed:
		j.ConfigMode = new(f.Mode())
	}
	maps.Copy(f.Table, want)

	if err := j.Save(c.dir); err != nil {
		return err
	}
	defer harness.PruneJournals(c.dir)
	// The endpoint carries the relay's token: the file is kept private.
	if err := f.Save(true); err != nil {
		var rollbackErr error
		if previous != nil {
			rollbackErr = previous.Save(c.dir)
		} else {
			rollbackErr = harness.DeleteJournal(c.dir, c.Name(), path)
		}
		if rollbackErr != nil {
			return fmt.Errorf("write %s: %w (also failed to restore its journal: %w)", path, err, rollbackErr)
		}
		return err
	}
	return nil
}

// Disconnect puts back what each journaled key held before, or takes it out, where it still
// holds what terma wrote; one changed since is the developer's and is named, not touched. A
// file that had no [telemetry] table comes back byte for byte.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil || j == nil {
		return harness.DisconnectResult{}, err
	}
	_, f, err := c.loadConfig()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	before := rendered(f.Table)
	after := maps.Clone(before)
	result, remaining := j.Apply(after)
	slices.Sort(result.Skipped)

	defer harness.PruneJournals(c.dir)
	if result.Removed+result.Restored > 0 {
		for key := range j.Installed {
			was, had := before[key]
			now, has := after[key]
			switch {
			case !has && had:
				delete(f.Table, key)
			case has && (!had || now != was):
				v, err := tomledit.Parse(now)
				if err != nil {
					return result, err
				}
				f.Table[key] = v
			}
		}
		// A key teardown leaves may still hold the relay's token: the file stays private
		// until the last one goes.
		if err := f.Save(!remaining.Empty()); err != nil {
			return result, err
		}
		if remaining.Empty() && j.ConfigMode != nil {
			if err := os.Chmod(path, *j.ConfigMode); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return result, err
			}
		}
	}
	if !remaining.Empty() {
		return result, remaining.Save(c.dir)
	}
	return result, harness.DeleteJournal(c.dir, c.Name(), path)
}

// CurrentCredential has none to reuse: the relay's token is the machine's, not a project's key.
func (exporter) CurrentCredential(string, string) (string, bool) { return "", false }

// Backup takes none: the journal restores each key terma wrote.
func (exporter) Backup(string) (string, error) { return "", nil }

// tokenPath reports whether endpoint is an URL with a path, the relay's token.
func tokenPath(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Host != "" && strings.Trim(u.Path, "/") != ""
}

var _ harness.Harness = exporter{}
