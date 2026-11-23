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
	// Accepted risk: a PI_CODING_AGENT_DIR that Pi and OMP share puts both terma extensions in one extensions/ folder, which each host loads; the upgrade path is a host check in each extension, once confirmed against Pi's source.
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

// Status reads back where omp's exporter sends traces: their own endpoint, else the common
// one. It is connected only where logs and metrics resolve to the same endpoint, as the
// extension requires before it asks OMP for content.
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

	// omp sends a signal to its own endpoint as given: one not ending in the signal's path
	// misses the relay's route.
	misrouted := false
	resolve := func(s harness.Signal) string {
		if per := strings.TrimRight(values[signalKey(s, "ENDPOINT")], "/"); per != "" {
			misrouted = misrouted || !strings.HasSuffix(per, "/v1/"+string(s))
			return strings.TrimSuffix(per, "/v1/"+string(s))
		}
		return strings.TrimRight(values[harness.EnvOTLPEndpoint], "/")
	}
	status.Endpoint = resolve(harness.SignalTraces)
	split := slices.ContainsFunc(harness.AllSignals, func(s harness.Signal) bool { return resolve(s) != status.Endpoint })
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
	// Traces alone name OMP's session: without them nothing it sends is attributed.
	status.Connected = !split && !misrouted && slices.Contains(status.Signals, harness.SignalTraces) && tokenPath(status.Endpoint)
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
	// retireUnwritten edits previous; a failed write restores the journal as it was.
	rollback := previous
	if previous != nil {
		kept := *previous
		kept.Installed, kept.Previous = maps.Clone(previous.Installed), maps.Clone(previous.Previous)
		rollback = &kept
	}
	changes := retireUnwritten(previous, current)
	installing := managed(e, current)
	// The journal keeps each entry's own text for teardown to put back, except where it still
	// holds what terma wrote, which the journal compares by value.
	existing := f.entries()
	if previous != nil {
		for key, installed := range previous.Installed {
			if v, ok := current[key]; ok && v == installed {
				existing[key] = v
			}
		}
	}
	j := harness.NewJournal(c.Name(), path, existing, installing, nil, nil, previous)

	for key, value := range installing {
		changes[key] = new(envLine(key, value))
	}
	if err := f.apply(changes); err != nil {
		return err
	}

	if err := j.Save(c.dir); err != nil {
		return err
	}
	defer harness.PruneJournals(c.dir)
	if err := f.save(); err != nil {
		var rollbackErr error
		if rollback != nil {
			rollbackErr = rollback.Save(c.dir)
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
				text, _ := journaledEntry(key, now)
				changes[key] = &text
			}
		}
		if err := f.apply(changes); err != nil {
			return result, err
		}
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

// managed is what a connect to e writes, given the file's current values: the common endpoint
// and protocol, and every signal's own endpoint, protocol and exporter (an exporter list that
// already names otlp kept as it is), with the SDK switched on. OMP fills a key this file leaves
// unset from the dotenv files it reads after it (its config root's, then the home directory's),
// so each key that could switch a signal off or send it elsewhere is set here, whether or not
// this file had it. The relay's token leads each endpoint's path, never a header: a
// repository's .env that sets a signal's endpoint outranks this file, and omp would send it
// this file's headers. Content capture is not here for the same reason: terma's extension asks
// for it in the running OMP, only where no endpoint outranks this file.
func managed(e harness.Exporter, current map[string]string) map[string]string {
	base := strings.TrimRight(e.Endpoint, "/") + "/" + e.APIKey
	out := map[string]string{
		harness.EnvOTLPEndpoint: base,
		harness.EnvOTLPProtocol: harness.ProtocolHTTPProtobuf,
		envSDKDisabled:          "false",
	}
	for _, s := range harness.AllSignals {
		out[signalKey(s, "ENDPOINT")] = harness.Exporter{Endpoint: base}.SignalEndpoint(s)
		out[signalKey(s, "PROTOCOL")] = harness.ProtocolHTTPProtobuf
		if v := current[signalExporterKey(s)]; strings.TrimSpace(v) == "" || !exportsOTLP(v) {
			out[signalExporterKey(s)] = exporterOTLP
		}
	}
	return out
}

// retireUnwritten undoes what an earlier setup wrote and this one does not: the Authorization
// headers that carried the relay's token, and content capture, which reached any endpoint that
// outranked the file. Each one still as written goes back to what it held, in current too, and
// the journal previous no longer owns any. It returns the file changes that makes.
func retireUnwritten(previous *harness.Journal, current map[string]string) map[string]*string {
	changes := map[string]*string{}
	if previous == nil {
		return changes
	}
	keys := []string{harness.EnvOTLPHeaders, ompCaptureContentEnv}
	for _, s := range harness.AllSignals {
		keys = append(keys, signalKey(s, "HEADERS"))
	}
	for _, key := range keys {
		installed, owned := previous.Installed[key]
		if !owned {
			continue
		}
		if current[key] == installed {
			if prior := previous.Previous[key]; prior != nil {
				text, value := journaledEntry(key, *prior)
				changes[key], current[key] = &text, value
			} else {
				changes[key] = nil
				delete(current, key)
			}
		}
		delete(previous.Installed, key)
		delete(previous.Previous, key)
	}
	return changes
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

// tokenPath reports whether endpoint names a path, the relay's token, ahead of /v1/<signal>.
func tokenPath(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && u.Host != "" && strings.Trim(u.Path, "/") != ""
}

var _ harness.Harness = exporter{}
