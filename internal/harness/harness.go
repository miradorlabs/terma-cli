// Package harness is the agent-neutral kit an exporter configuration is built from: the
// Exporter a connect describes, the Harness interface, connect scope, the ownership
// journal, symlink-safe settings writes, and the headers helper.
package harness

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/miradorlabs/terma-cli/internal/account/serverkey"
)

// Signal is one OTLP telemetry stream.
type Signal string

// The three streams, spelled as --signals accepts them and as OTLP names them.
const (
	SignalTraces  Signal = "traces"
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
)

// AllSignals is the default, in the order signals are reported.
var AllSignals = []Signal{SignalTraces, SignalLogs, SignalMetrics}

// ParseSignals parses --signals; an unknown name is an error, since a typo would look connected and emit nothing.
func ParseSignals(raw string) ([]Signal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AllSignals, nil
	}
	// "none" is a policy: every exporter is written off rather than left to inherit.
	if strings.EqualFold(raw, "none") {
		return []Signal{}, nil
	}

	seen := map[Signal]bool{}
	for part := range strings.SplitSeq(raw, ",") {
		name := Signal(strings.ToLower(strings.TrimSpace(part)))
		if name == "" {
			continue
		}
		switch name {
		case SignalTraces, SignalLogs, SignalMetrics:
			seen[name] = true
		default:
			return nil, fmt.Errorf("unknown signal %q (want traces, logs, or metrics)", part)
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("--signals was empty (want traces, logs, or metrics)")
	}

	out := make([]Signal, 0, len(seen))
	for _, s := range AllSignals {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out, nil
}

// Exporter is the agent-neutral description of where telemetry goes and how much of it.
type Exporter struct {
	// Endpoint is the OTLP base URL; the agent's SDK appends the signal paths.
	Endpoint string
	// APIKey is the server key; it lands in a config file, so writers tighten its mode.
	APIKey string
	// Signals are the streams to turn on; the rest are written off, not left unset.
	Signals []Signal
	// ResourceAttributes are stamped on everything the agent emits, where it supports them.
	ResourceAttributes map[string]string
	// ProjectID is recorded in the connect journal, since the telemetry may not name it.
	ProjectID string

	// HelperPath, when set, is a 0700 script that prints the Authorization header, so the
	// settings file holds a path instead of the key. Empty means inline delivery.
	HelperPath string
}

// An Exporter carries no content switch: every agent sends prompts and tool content to
// the local relay, which withholds what the team's collection policy does not collect.

// HasSignal reports whether a stream is enabled.
func (e Exporter) HasSignal(s Signal) bool {
	return slices.Contains(e.Signals, s)
}

// SignalEndpoint is the full per-signal URL: OTLP uses a per-signal endpoint as-is, so the
// bare base URL there would post to the wrong path.
func (e Exporter) SignalEndpoint(s Signal) string {
	if e.Endpoint == "" {
		return ""
	}
	return strings.TrimRight(e.Endpoint, "/") + "/v1/" + string(s)
}

// ResourceAttributesValue renders OTEL_RESOURCE_ATTRIBUTES, sorted so a connect is
// byte-stable, dropping pairs whose comma or equals sign would corrupt the encoding.
func (e Exporter) ResourceAttributesValue() string {
	keys := make([]string, 0, len(e.ResourceAttributes))
	for k, v := range e.ResourceAttributes {
		if k == "" || v == "" {
			continue
		}
		if strings.ContainsAny(k, ",=") || strings.ContainsAny(v, ",=") {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+e.ResourceAttributes[k])
	}
	return strings.Join(pairs, ",")
}

// Detection is what the CLI learnt about an installed agent; not found is not an error.
type Detection struct {
	Found   bool
	Version string
	Path    string
}

// Conflict is an existing setting that would send Terma's credential elsewhere or stop
// the export: a per-signal OTLP endpoint wins over the generic one and would receive its
// Authorization header.
type Conflict struct {
	Key string
	// Value is never a credential: only endpoint and protocol keys report one.
	Value  string
	Reason string
	// Credential is true when leaving this in place would disclose Terma's key.
	Credential bool

	// Scope is where the setting was found; only the agent's user-level file is Terma's to edit.
	Scope string
	// Clearable reports whether Connect could remove it; one that is not is fatal even under --force.
	Clearable bool

	// Advisory marks a setting that applies only in a mode the user selects, so it is
	// reported but neither blocks a connect nor marks the agent overridden.
	Advisory bool
}

// Conflict scopes.
const (
	ScopeUserSettings = "user settings"
	ScopeEnvironment  = "shell environment"
	ScopeProject      = "project settings"
	// ScopeManaged is an administrator-managed layer that outranks everything the user writes.
	ScopeManaged = "managed settings"
	// ScopeProfile is a profile file applied over the user config while it is selected.
	ScopeProfile = "profile settings"
)

// DisconnectResult reports what a disconnect did.
type DisconnectResult struct {
	Removed  int
	Restored int
	// Skipped names keys changed since Terma wrote them, left as someone's deliberate edit.
	Skipped []string
	// Unjournaled is set when no connect record survived, so keys were removed, not restored.
	Unjournaled bool
}

// Status is the installed state, read back from the agent's own config.
type Status struct {
	// HasPolicy reports explicit repository export settings, journal or not.
	HasPolicy  bool
	ConfigPath string
	Exists     bool
	// Connected is true only when the agent points at a Terma OTLP endpoint.
	Connected bool
	Endpoint  string
	Signals   []Signal
	// StaleContent reports a repository setting an earlier terma wrote to withhold content;
	// install removes it, since only the team's policy decides content.
	StaleContent bool

	// ManagedKeys counts Terma-written keys present; disconnect keys off it, not Connected,
	// because telemetry switched off with the key still on disk most needs cleaning.
	ManagedKeys int

	Conflicts []Conflict

	// KeyPrefix is the masked head of the configured key, never the key itself.
	KeyPrefix string
	// ProjectID is the project the agent actually reports to, not the selected one.
	ProjectID string
}

// Harness is one configurable agent CLI.
type Harness interface {
	// Name is the command-line token: `terma telemetry connect <name>`.
	Name() string
	// DisplayName is how it is written in prose.
	DisplayName() string

	Detect(ctx context.Context) Detection

	// ConfigPath is the file Connect and Disconnect write.
	ConfigPath() (string, error)

	// SupportsHeadersHelper reports whether the agent can read OTLP headers from a
	// script; without it the key is written inline and HelperPath is ignored.
	SupportsHeadersHelper() bool

	Status() (Status, error)

	// ConflictsWith reports settings that would redirect or override e, before Connect
	// writes the credential; it takes the Exporter because conflicts depend on the signals.
	ConflictsWith(e Exporter) ([]Conflict, error)

	// Connect installs the exporter, preserving settings it does not own; conflicting
	// overrides are the user's and are removed only when clearConflicts is set.
	Connect(e Exporter, clearConflicts bool) error

	// Disconnect restores keys still holding what Terma wrote and leaves changed ones alone.
	Disconnect() (DisconnectResult, error)

	// Local is the exporter bound to the repository at root's settings file; false for an
	// agent with no repository scope. It writes nothing.
	Local(root string) (Harness, bool)
	// CurrentCredential reads back the key configured for endpoint and project, so a
	// reconnect reuses it; keys are per agent so one can be revoked alone.
	CurrentCredential(endpoint, projectID string) (key string, ok bool)
	// Backup snapshots the configuration before a connect; "" for none taken.
	Backup(endpoint string) (path string, err error)
	// ConnectNotes are said before the developer confirms a connect.
	ConnectNotes(e Exporter) []string
}

// ErrUnsupported is returned by an agent that is registered but not yet implemented.
type ErrUnsupported struct {
	Harness string
	Reason  string
}

func (e *ErrUnsupported) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("%s telemetry is not supported yet — %s", e.Harness, e.Reason)
	}
	return fmt.Sprintf("%s telemetry is not supported yet", e.Harness)
}

// SignalNames is signals as the words the config files hold.
func SignalNames(signals []Signal) []string {
	out := make([]string, 0, len(signals))
	for _, s := range signals {
		out = append(out, string(s))
	}
	return out
}

// SignalsFromNames is the signals names spell, in AllSignals order, dropping unknown words.
func SignalsFromNames(names []string) []Signal {
	var out []Signal
	for _, s := range AllSignals {
		for _, n := range names {
			if Signal(strings.ToLower(strings.TrimSpace(n))) == s {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

// WithoutSignal is signals without drop.
func WithoutSignal(signals []Signal, drop Signal) []Signal {
	out := signals[:0]
	for _, s := range signals {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}

// MaskKey renders a credential as a recognizable but unusable prefix.
func MaskKey(key string) string {
	if key == "" {
		return ""
	}
	return serverkey.Mask(key)
}

// Partition splits conflicts into those that gate a connect and the advisory rest.
func Partition(conflicts []Conflict) (blocking, advisory []Conflict) {
	for _, c := range conflicts {
		if c.Advisory {
			advisory = append(advisory, c)
		} else {
			blocking = append(blocking, c)
		}
	}
	return blocking, advisory
}

// LocalOffOnly is a harness whose agent ignores repository settings that turn telemetry
// on, so its repository scope can only switch signals and content off.
type LocalOffOnly interface {
	LocalOffOnly()
}
