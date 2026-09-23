// Package harness configures agent CLIs — Claude Code, Codex and OpenCode — to export
// OpenTelemetry to Terma.
//
// Each harness owns exactly one thing: the translation between a Terma Exporter and
// whatever configuration file and setting names that vendor happens to use. Nothing
// outside this package knows that Claude Code reads ~/.claude/settings.json and spells
// its telemetry switch CLAUDE_CODE_ENABLE_TELEMETRY, or that Codex reads an `[otel]`
// table in ~/.codex/config.toml, which is what keeps `terma telemetry` a single
// command tree rather than one per vendor.
//
// Inside each harness the translation (render) is kept apart from the write (Connect):
// render is pure, so what a connect would put on disk — including which redaction
// switches are off — can be computed, compared with what is there and tested without
// touching a file. It is not part of the interface: the three harnesses render three
// different things (environment variables, a TOML table, a plugin's config) and nothing
// outside the package has a use for any of them.
package harness

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Signal is one OTLP telemetry stream.
type Signal string

// The three streams, spelled as --signals accepts them and as OTLP names them.
const (
	SignalTraces  Signal = "traces"
	SignalLogs    Signal = "logs"
	SignalMetrics Signal = "metrics"
)

// AllSignals is the default: everything Terma can ingest. Ordered most- to
// least-structural, which is also the order they are reported in.
var AllSignals = []Signal{SignalTraces, SignalLogs, SignalMetrics}

// ParseSignals turns a comma-separated --signals value into a deduplicated,
// canonically-ordered list. An unknown name is an error rather than a silent drop: a
// typo'd signal would otherwise look connected and emit nothing.
func ParseSignals(raw string) ([]Signal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AllSignals, nil
	}
	// "none" is a policy, not an absence: a repository that ships nothing says so
	// explicitly, and every exporter is written off rather than left to inherit.
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

// Exporter is the vendor-neutral description of where a harness should send telemetry
// and how much of it to include.
type Exporter struct {
	// Endpoint is the OTLP base URL. Signal paths (/v1/traces …) are appended by the
	// harness's own OTel SDK, not here.
	Endpoint string
	// APIKey is the server key (ter_srv_…) the harness presents. It ends up inside a config file
	// on disk, which is why writers in this package tighten that file's mode.
	APIKey string
	// Signals are the streams to turn on. Streams not listed are explicitly disabled
	// rather than left unset, so the resulting config is unambiguous.
	Signals []Signal
	// ResourceAttributes are stamped on everything the harness emits. This is how a
	// trace gets attributed to a person and a Terma project. Codex and OpenCode carry
	// them; Claude Code does not (see renderClaude).
	ResourceAttributes map[string]string
	// ProjectID is the Terma project this export reports to. It is recorded in the
	// connect journal, which is how status and key reuse learn which project a
	// configuration belongs to when the telemetry itself does not say.
	ProjectID string

	// HelperPath, when set, delivers the credential through the harness's headers-helper
	// mechanism instead of an inline header: connect writes a 0700 script at this path
	// that prints the Authorization header, and points the harness's otelHeadersHelper
	// at it. The settings file then never holds the key — only a path — so it stays
	// shareable, diffable, and safe in a dotfiles repo. Empty means inline delivery.
	HelperPath string

	// IncludePrompts and IncludeToolContent control content capture. They are separate
	// switches because they disclose different things: prompts and responses are what
	// the user and model said, tool content is what ran and what came back. The policy
	// default (capture on, excluded by flag) belongs to the connect command; this struct
	// just records what was decided, and its zero value captures nothing.
	IncludePrompts     bool
	IncludeToolContent bool
}

// HasSignal reports whether a stream is enabled.
func (e Exporter) HasSignal(s Signal) bool {
	return slices.Contains(e.Signals, s)
}

// SignalEndpoint is the full URL a per-signal OTLP variable must carry to reach the
// same place the generic endpoint does.
//
// The OTLP spec makes these two different: an exporter appends `v1/traces` (and so on)
// to OTEL_EXPORTER_OTLP_ENDPOINT, but a per-signal OTEL_EXPORTER_OTLP_<signal>_ENDPOINT
// "MUST be used as-is without any modification". So a per-signal variable holding the
// bare base URL is not equivalent to the generic one — it posts to the wrong path — and
// only the value below is actually safe.
func (e Exporter) SignalEndpoint(s Signal) string {
	if e.Endpoint == "" {
		return ""
	}
	return strings.TrimRight(e.Endpoint, "/") + "/v1/" + string(s)
}

// ResourceAttributesValue renders OTEL_RESOURCE_ATTRIBUTES. Keys are sorted so the
// value is byte-stable across runs — an unstable ordering would make every connect
// look like a change to the config file.
//
// Values containing a comma or an equals sign would corrupt the W3C Baggage-style
// encoding this variable uses, so they are dropped rather than written out broken.
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
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+e.ResourceAttributes[k])
	}
	return strings.Join(pairs, ",")
}

// Detection is what the CLI could learn about an installed harness. NotFound is not an
// error: connecting a harness that is not installed yet is legitimate — the config is
// read at its next start, whenever that is.
type Detection struct {
	Found   bool
	Version string
	// Path is the executable that answered, for a status line that can say which one.
	Path string
}

// Conflict is an existing setting that would send Terma's credential somewhere
// Terma does not control, or would silently stop the export from working.
//
// These exist because OTLP is configurable per signal as well as generically, and the
// per-signal values win. A harness carrying its own OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// would keep exporting there while receiving the Authorization header Terma wrote for
// the generic endpoint — handing a live server key to a third-party collector.
type Conflict struct {
	// Key is the environment variable in the harness's config.
	Key string
	// Value is what it is currently set to. Never a credential: only keys whose values
	// are endpoints or protocols are reported with one.
	Value string
	// Reason says what it would do, in terms a user can act on.
	Reason string
	// Credential is true when leaving this in place would disclose Terma's key.
	Credential bool

	// Scope is where the setting was found. Only the harness's own user-level file is
	// Terma's to edit; a shell export or a project settings file belongs to someone
	// else, and both take precedence over it.
	Scope string
	// Clearable reports whether Connect could remove this. A conflict that is not
	// clearable is fatal even under --force: pretending to fix it would write the
	// credential and leave the override in force.
	Clearable bool

	// Advisory marks a setting that only takes effect in a mode the user has to select
	// explicitly — a Codex profile — so whether it applies to the next session cannot be
	// known here. It is reported so the user can decide, but it neither blocks a connect
	// nor needs --force, and status does not call the harness overridden for it.
	Advisory bool
}

// Conflict scopes.
const (
	ScopeUserSettings = "user settings"
	ScopeEnvironment  = "shell environment"
	ScopeProject      = "project settings"
	// ScopeManaged is an administrator-managed layer that outranks everything the user
	// writes — Codex's /etc/codex/managed_config.toml, or its macOS managed preference.
	ScopeManaged = "managed settings"
	// ScopeProfile is a Codex profile file, `<name>.config.toml`, applied over the user
	// config while that profile is selected.
	ScopeProfile = "profile settings"
)

// DisconnectResult reports what a disconnect actually did. Removed and Restored are
// counts of managed keys; Skipped names the ones deliberately left in place.
type DisconnectResult struct {
	// Removed is the count of Terma keys taken out of the config.
	Removed int
	// Restored is the count put back to the value they held before Terma ran.
	Restored int
	// Skipped names keys changed since Terma wrote them. They are somebody's
	// deliberate edit, so disconnect leaves them and says which.
	Skipped []string
	// Unjournaled is set when no record of the connect survived, so prior values could
	// not be restored and the keys were simply removed.
	Unjournaled bool
}

// Status is the currently-installed state, read back from the harness's own config.
type Status struct {
	// HasPolicy reports explicit repository export settings, including disabled
	// signals. Unlike ManagedKeys, it does not depend on Terma's ownership journal.
	HasPolicy bool
	// ConfigPath is the file consulted, whether or not it exists.
	ConfigPath string
	Exists     bool
	// Connected is true only when this harness points at a Terma OTLP endpoint.
	// A harness exporting to someone else's collector is deliberately not "connected".
	Connected bool
	Endpoint  string
	Signals   []Signal

	IncludePrompts     bool
	IncludeToolContent bool

	// ManagedKeys counts the Terma-written variables actually present. Disconnect
	// keys off this rather than off Connected: telemetry switched off with the key
	// still on disk is exactly the state that most needs cleaning up.
	ManagedKeys int

	// Conflicts are per-signal overrides that contradict the generic configuration.
	Conflicts []Conflict

	// KeyPrefix is the masked head of the configured key, never the key itself.
	KeyPrefix string
	// ProjectID is read back from the stamped resource attributes, so status can say
	// which project the harness actually reports to rather than which one is selected.
	ProjectID string
}

// Harness is one configurable agent CLI.
type Harness interface {
	// Name is the command-line token: `terma telemetry connect <name>`.
	Name() string
	// DisplayName is how it is written in prose — "Claude Code".
	DisplayName() string

	// Detect looks for an installed binary and its version.
	Detect(ctx context.Context) Detection

	// ConfigPath is the file Connect and Disconnect write.
	ConfigPath() (string, error)

	// SupportsHeadersHelper reports whether the harness can fetch its OTLP headers from
	// a script at startup, so the credential can live outside its config file. Without
	// it the key is written inline and the file's mode is tightened instead; the
	// Exporter's HelperPath is then ignored.
	SupportsHeadersHelper() bool

	// Status reads the config file back.
	Status() (Status, error)

	// ConflictsWith reports settings already in the config that would override or
	// redirect the export e describes. Called before Connect writes anything, because
	// the dangerous case — a per-signal endpoint inheriting Terma's Authorization
	// header — is invisible once the credential is on disk.
	//
	// It takes the whole Exporter rather than just an endpoint because whether a setting
	// conflicts depends on which signals are enabled: a redirect that only moves traces
	// is irrelevant to a metrics-only connect.
	ConflictsWith(e Exporter) ([]Conflict, error)

	// Connect installs the exporter: renders it, merges the result into the config, and
	// (in helper mode) writes the credential script — preserving every setting it does
	// not own. Conflicting overrides are removed only when clearConflicts is set, which
	// the caller gates behind an explicit flag: they are the user's settings, not
	// Terma's.
	Connect(e Exporter, clearConflicts bool) error

	// Disconnect undoes a connect: keys Terma installed and that still hold the value
	// it wrote are restored to what they were before, and keys somebody has since
	// changed are left alone.
	Disconnect() (DisconnectResult, error)
}

// registry is fixed at compile time. A harness is a code-level integration — it has to
// know a vendor's file format and variable names — so there is nothing a runtime
// registration would enable.
var registry = []Harness{Claude{}, Codex{}, OpenCode{}}

// All returns every known harness, in the order they are listed by `--help`.
func All() []Harness {
	out := make([]Harness, len(registry))
	copy(out, registry)
	return out
}

// Names lists the tokens accepted by connect/status/disconnect.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, h := range registry {
		out = append(out, h.Name())
	}
	return out
}

// Lookup resolves a command-line token to a harness.
func Lookup(name string) (Harness, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	for _, h := range registry {
		if h.Name() == want {
			return h, nil
		}
	}
	return nil, fmt.Errorf("unknown agent %q (want %s)", name, strings.Join(Names(), " or "))
}

// ErrUnsupported is returned by a harness that is registered but not yet implemented,
// so `--help` can name it before it works. No registered harness returns it today; it
// stays so the next one can be listed before it is finished.
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
