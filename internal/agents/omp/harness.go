package omp

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// exporter configures omp's native OTLP exporter through the agent directory's .env, which
// omp reads OTEL_* from privately: no tool, shell or extension omp runs inherits it. The
// keys terma writes win over the developer's own lines for them, and what they held is
// journaled under terma's config directory dir, so a disconnect restores it.
type exporter struct {
	dir string
}

const (
	envFileName = ".env"

	// omp's agent directory: PI_CODING_AGENT_DIR replaces it (in the default profile), and
	// PI_CONFIG_DIR renames ~/.omp.
	envAgentDir  = "PI_CODING_AGENT_DIR"
	envConfigDir = "PI_CONFIG_DIR"

	ompCaptureContentEnv  = "OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"
	ompCaptureContentFull = "full"
	envSDKDisabled        = "OTEL_SDK_DISABLED"

	exporterOTLP = "otlp"
	exporterNone = "none"
)

// signalKey is the per-signal form of an OTEL_EXPORTER_OTLP_* key, which omp reads ahead of
// the common one: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT for traces and "ENDPOINT".
func signalKey(s harness.Signal, suffix string) string {
	return "OTEL_EXPORTER_OTLP_" + strings.ToUpper(string(s)) + "_" + suffix
}

// signalExporterKey is OTEL_TRACES_EXPORTER and its kin; a list that does not name otlp, or names none, switches the signal off.
func signalExporterKey(s harness.Signal) string {
	return "OTEL_" + strings.ToUpper(string(s)) + "_EXPORTER"
}

// Name is the token `--harness` accepts.
func (exporter) Name() string { return name }

// DisplayName is how the agent is written in prose.
func (exporter) DisplayName() string { return displayName }

// Local is unsupported: omp reads its exporter from the developer's own files only.
func (exporter) Local(string) (harness.Harness, bool) { return nil, false }

// Detect runs `omp --version`; a missing binary is not-found, not an error.
func (exporter) Detect(ctx context.Context) harness.Detection {
	return harness.DetectBinary(ctx, "omp", harness.SemverRE)
}

// ompAgentDir is where omp keeps its user configuration: $PI_CODING_AGENT_DIR, else
// ~/$PI_CONFIG_DIR/agent, ~/.omp/agent by default. A named profile (omp --profile) has its
// own directory, which terma does not configure.
func ompAgentDir() (string, error) {
	if dir := os.Getenv(envAgentDir); dir != "" {
		return filepath.Abs(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	root := os.Getenv(envConfigDir)
	if root == "" {
		root = ".omp"
	}
	return filepath.Join(home, root, "agent"), nil
}

// ConfigPath is the agent directory's .env.
func (exporter) ConfigPath() (string, error) {
	dir, err := ompAgentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, envFileName), nil
}

// Status reads back where omp's exporter sends: the common endpoint, or a per-signal one
// that differs from it, which is where that signal's records and the token go.
func (c exporter) Status() (harness.Status, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.Status{}, err
	}
	f, err := loadEnvFile(path)
	if err != nil {
		return harness.Status{}, err
	}
	values := f.values()
	status := harness.Status{ConfigPath: path, Exists: f.existed}

	base := strings.TrimRight(values[harness.EnvOTLPEndpoint], "/")
	status.Endpoint = base
	for _, s := range harness.AllSignals {
		per := strings.TrimRight(values[signalKey(s, "ENDPOINT")], "/")
		if per != "" && strings.TrimSuffix(per, "/v1/"+string(s)) != base {
			status.Endpoint = per
			break
		}
	}
	disabled := strings.EqualFold(strings.TrimSpace(values[envSDKDisabled]), "true")
	for _, s := range harness.AllSignals {
		protocol := values[signalKey(s, "PROTOCOL")]
		if protocol == "" {
			protocol = values[harness.EnvOTLPProtocol]
		}
		if !disabled && exportsOTLP(values[signalExporterKey(s)]) && (protocol == "" || protocol == harness.ProtocolHTTPProtobuf) {
			status.Signals = append(status.Signals, s)
		}
	}
	status.Connected = status.Endpoint != "" && len(status.Signals) > 0 && bearer(values[harness.EnvOTLPHeaders]) != ""
	status.Conflicts = ompConflicts(harness.Exporter{Endpoint: status.Endpoint})

	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return harness.Status{}, err
	}
	if j != nil {
		for key, installed := range j.Installed {
			if v, ok := values[key]; ok && v == installed {
				status.ManagedKeys++
			}
		}
	}
	return status, nil
}

// ConflictsWith reports shell exports that would defeat or redirect e.
func (c exporter) ConflictsWith(e harness.Exporter) ([]harness.Conflict, error) {
	return ompConflicts(e), nil
}

// Connect writes terma's keys into the .env, leaving every other line as it was; what each
// key held before is journaled first, so a failed write leaves nothing unrecorded.
func (c exporter) Connect(e harness.Exporter, _ bool) error {
	path, err := c.ConfigPath()
	if err != nil {
		return err
	}
	f, err := loadEnvFile(path)
	if err != nil {
		return err
	}
	previous, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil {
		return err
	}
	current := f.values()
	installing := managed(e, current)
	j := harness.NewJournal(c.Name(), path, current, installing, nil, nil, previous)

	changes := map[string]*string{}
	for key, value := range installing {
		changes[key] = &value
	}
	f.apply(changes)

	if err := j.Save(c.dir); err != nil {
		return err
	}
	defer harness.PruneJournals(c.dir)
	if err := f.save(); err != nil {
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

// Disconnect puts back what each journaled key held before, where it still holds what terma
// wrote; one changed since is the developer's and is named, not touched.
func (c exporter) Disconnect() (harness.DisconnectResult, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	j, err := harness.LoadJournal(c.dir, c.Name(), path)
	if err != nil || j == nil {
		return harness.DisconnectResult{}, err
	}
	f, err := loadEnvFile(path)
	if err != nil {
		return harness.DisconnectResult{}, err
	}
	before := f.values()
	after := maps.Clone(before)
	result, remaining := j.Apply(after)
	slices.Sort(result.Skipped)

	defer harness.PruneJournals(c.dir)
	if result.Removed+result.Restored > 0 {
		changes := map[string]*string{}
		for key := range j.Installed {
			was, had := before[key]
			now, has := after[key]
			switch {
			case !has && had:
				changes[key] = nil
			case has && (!had || now != was):
				changes[key] = &now
			}
		}
		f.apply(changes)
		if err := f.save(); err != nil {
			return result, err
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

// managed is what a connect to e writes, given the file's current values: the common
// endpoint, protocol, Authorization header and content capture, every signal's own endpoint
// (a per-signal endpoint in a dotenv file omp reads after this one would otherwise outrank
// the common one, and receive the relay's token), and the per-signal settings already in
// the file that would redirect or switch off a signal.
func managed(e harness.Exporter, current map[string]string) map[string]string {
	auth := "authorization=Bearer%20" + url.QueryEscape(e.APIKey)
	out := map[string]string{
		harness.EnvOTLPEndpoint: e.Endpoint,
		harness.EnvOTLPProtocol: harness.ProtocolHTTPProtobuf,
		harness.EnvOTLPHeaders:  auth,
		ompCaptureContentEnv:    ompCaptureContentFull,
	}
	for _, s := range harness.AllSignals {
		out[signalKey(s, "ENDPOINT")] = e.SignalEndpoint(s)
		if _, ok := current[signalKey(s, "HEADERS")]; ok {
			out[signalKey(s, "HEADERS")] = auth
		}
		if _, ok := current[signalKey(s, "PROTOCOL")]; ok {
			out[signalKey(s, "PROTOCOL")] = harness.ProtocolHTTPProtobuf
		}
		if v, ok := current[signalExporterKey(s)]; ok && !exportsOTLP(v) {
			out[signalExporterKey(s)] = exporterOTLP
		}
	}
	if v, ok := current[envSDKDisabled]; ok && strings.EqualFold(strings.TrimSpace(v), "true") {
		out[envSDKDisabled] = "false"
	}
	return out
}

// exportsOTLP reports whether omp exports a signal whose exporter list is list: an empty one
// does, one naming none does not, and otherwise one must name otlp.
func exportsOTLP(list string) bool {
	otlp, named := false, false
	for e := range strings.SplitSeq(list, ",") {
		switch strings.ToLower(strings.TrimSpace(e)) {
		case "":
			continue
		case exporterNone:
			return false
		case exporterOTLP:
			otlp = true
		}
		named = true
	}
	return otlp || !named
}

// bearer is the token an OTLP headers list sends as `authorization=Bearer <token>`, its
// values percent-encoded as omp decodes them.
func bearer(headers string) string {
	for pair := range strings.SplitSeq(headers, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "authorization") {
			continue
		}
		if decoded, err := url.PathUnescape(strings.TrimSpace(v)); err == nil {
			v = decoded
		}
		if token, ok := strings.CutPrefix(v, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	return ""
}

var _ harness.Harness = exporter{}
