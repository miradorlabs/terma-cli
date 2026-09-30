package claude

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Claude Code resolves a setting from several files, and the user-level one Terma
// writes is the *lowest* precedence of them: managed settings, then `--settings`, then
// `.claude/settings.local.json`, then `.claude/settings.json`, then
// `~/.claude/settings.json`. An `env` block is an ordinary key and follows that order.
//
// So checking only the file Terma writes is not enough to know where telemetry will
// actually go. A project file — or a variable exported in the shell — can define
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT and win, while the user file supplies the generic
// Authorization header carrying Terma's key. Neither of those is Terma's to edit, so
// they are reported as conflicts that --force cannot clear and connect must refuse.
//
// This is a best-effort scan of the directory the CLI happens to be run from. Claude Code
// may later run somewhere else entirely, with different project settings; that is stated
// in the docs rather than papered over here.
//
// A repository-scope connect writes one of those project files itself. What outranks
// *that* is narrower — the same repository's settings.local.json, and the shell — and
// the user file below it is not a conflict at all: the project file is meant to win.

// claudeLayer is the file a Claude value writes, seen from conflict detection: what to
// call a setting found in the file itself, how to name the file when an outranking
// setting overrides it, and which project files Claude Code applies over it.
type claudeLayer struct {
	scope      string
	over       string
	outranking []string
}

// scopeRepositorySettings labels a conflict found in the repository's own
// .claude/settings.json during a repository-scope connect. Like ScopeUserSettings it is
// the file Terma is writing, so such a conflict is clearable with --force.
const scopeRepositorySettings = "repository settings"

// layer describes the file this value writes.
func (c exporter) layer() claudeLayer {
	if c.root != "" {
		return claudeLayer{
			scope: scopeRepositorySettings,
			over:  "this repository's " + claudeProjectSettings,
			outranking: []string{
				filepath.Join(c.root, filepath.FromSlash(claudeProjectLocalSettings)),
			},
		}
	}
	return claudeLayer{
		scope:      harness.ScopeUserSettings,
		over:       "your user settings",
		outranking: projectFilesAbove(),
	}
}

// projectFilesAbove lists the project settings files Claude Code applies over the user
// file, for the directory the CLI was run from: both files at each level, local before
// shared to match Claude Code's own precedence, walking up to the repository root so a
// subdirectory of a repository still finds the project's settings.
func projectFilesAbove() []string {
	dir, err := os.Getwd()
	if err != nil {
		return nil
	}
	var out []string
	for {
		for _, name := range []string{"settings.local.json", "settings.json"} {
			out = append(out, filepath.Join(dir, ".claude", name))
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			break // repository root
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // filesystem root
		}
		dir = parent
	}
	return out
}

// redirectKeys are the settings that decide where telemetry goes, and therefore the ones
// worth hunting for outside the user file.
func redirectKeys() []string {
	keys := []string{harness.EnvOTLPEndpoint, harness.EnvOTLPHeaders, claudeBetaTracingEndpoint}
	for _, o := range perSignalOverrides {
		keys = append(keys, o.endpoint, o.headers, o.protocol)
	}
	return keys
}

// environmentConflicts reports redirect settings exported in the process environment.
//
// Whether a settings-file `env` entry beats an already-exported shell variable is not
// documented for the OTEL_* names, so this refuses to guess: an exported value that
// disagrees with what Terma is about to install is reported, and the user is told to
// unset it. Being wrong in the other direction would mean writing a credential into a
// config whose destination is decided elsewhere.
func environmentConflicts(e harness.Exporter) []harness.Conflict {
	var out []harness.Conflict
	for _, key := range redirectKeys() {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if !relevantToExporter(key, e) {
			continue
		}
		if expected, ok := expectedValue(key, e); ok && value == expected {
			continue
		}
		out = append(out, harness.Conflict{
			Key:        key,
			Value:      redactIfHeader(key, value),
			Reason:     "exported in your shell, where it takes effect regardless of what is written to the settings file",
			Credential: isRedirect(key),
			Scope:      harness.ScopeEnvironment,
			Clearable:  false,
		})
	}
	return out
}

// projectConflicts reports redirect settings in the project files that outrank the file
// l describes.
func projectConflicts(e harness.Exporter, l claudeLayer) []harness.Conflict {
	var out []harness.Conflict
	for _, path := range l.outranking {
		out = append(out, conflictsInProjectFile(path, e, l)...)
	}
	return out
}

func conflictsInProjectFile(path string, e harness.Exporter, l claudeLayer) []harness.Conflict {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Env               map[string]string `json:"env"`
		OtelHeadersHelper string            `json:"otelHeadersHelper"`
	}
	if json.Unmarshal(data, &doc) != nil {
		// A project file this CLI cannot parse is not this CLI's to complain about; the
		// user file is still checked, and Claude Code will report its own parse error.
		return nil
	}

	var out []harness.Conflict
	for _, key := range redirectKeys() {
		value, ok := doc.Env[key]
		if !ok || value == "" || !relevantToExporter(key, e) {
			continue
		}
		if expected, ok := expectedValue(key, e); ok && value == expected {
			continue
		}
		out = append(out, harness.Conflict{
			Key:        key,
			Value:      redactIfHeader(key, value),
			Reason:     "set in " + path + ", which Claude Code applies over " + l.over,
			Credential: isRedirect(key),
			Scope:      harness.ScopeProject,
			Clearable:  false,
		})
	}
	if doc.OtelHeadersHelper != "" {
		out = append(out, harness.Conflict{
			Key:        claudeOtelHeadersHelper,
			Value:      doc.OtelHeadersHelper,
			Reason:     "set in " + path + ", and supplies its own OTLP headers over " + l.over,
			Credential: true,
			Scope:      harness.ScopeProject,
			Clearable:  false,
		})
	}
	return out
}

// captureKeys are the content-capture switches. They do not decide where telemetry
// goes, so they are not redirectKeys and cannot leak the credential — but a
// higher-precedence "off" silently empties every view built on prompt and tool
// content while `telemetry status` still reports the harness connected. That silence
// is the whole problem, so they are scanned in the outranking scopes too.
var captureKeys = []string{
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// intendsCapture reports whether e turns a given capture key on.
func intendsCapture(key string, e harness.Exporter) bool {
	switch key {
	case otelLogUserPrompts, otelLogAssistantResponse:
		return e.IncludePrompts
	case otelLogToolDetails, otelLogToolContent:
		return e.IncludeToolContent
	}
	return false
}

// captureConflictsIn reports content capture that an outranking scope turns off while
// this export means to turn it on.
//
// Advisory, always: these disclose nothing and break no export, so they must not block
// a connect or need --force. They exist to be said out loud, because the alternative is
// a connect that reports success and a dashboard that stays empty.
func captureConflictsIn(e harness.Exporter, l claudeLayer) []harness.Conflict {
	var out []harness.Conflict
	for _, key := range captureKeys {
		if !intendsCapture(key, e) {
			continue // nothing is being overridden if Terma is not asking for it
		}
		if value := os.Getenv(key); value != "" && !isOn(value) {
			out = append(out, harness.Conflict{
				Key:       key,
				Value:     value,
				Reason:    "exported in your shell as off, which suppresses this content however the settings file is written",
				Scope:     harness.ScopeEnvironment,
				Clearable: false,
				Advisory:  true,
			})
		}
	}
	out = append(out, captureConflictsInProjectFiles(e, l)...)
	return out
}

// captureConflictsInProjectFiles is captureConflictsIn for the project settings that
// outrank the file l describes.
//
// A value Terma itself put there — a repository's local layer, written by
// `terma connect claude --scope local` — is still reported, because it still decides
// what leaves the machine from this repository; but it is named as the repository's
// policy rather than as a stray override, so the reader knows which command set it.
func captureConflictsInProjectFiles(e harness.Exporter, l claudeLayer) []harness.Conflict {
	var out []harness.Conflict
	for _, path := range l.outranking {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			Env map[string]string `json:"env"`
		}
		if json.Unmarshal(data, &doc) != nil {
			continue
		}
		owned := termaOwnedKeys(path, doc.Env)
		for _, key := range captureKeys {
			value, ok := doc.Env[key]
			if !ok || value == "" || isOn(value) || !intendsCapture(key, e) {
				continue
			}
			reason := "set off in " + path + ", which Claude Code applies over " + l.over
			if owned[key] {
				reason = "turned off by this repository's Terma policy in " + path +
					" (change it with `terma connect claude --scope local`)"
			}
			out = append(out, harness.Conflict{
				Key:       key,
				Value:     value,
				Reason:    reason,
				Scope:     harness.ScopeProject,
				Clearable: false,
				Advisory:  true,
			})
		}
	}
	return out
}

// termaOwnedKeys reports which env keys in a project file still hold what a Terma
// connect installed there, according to this machine's journal for that file. Best
// effort: a colleague's committed layer has no journal here and reads as unowned,
// which only changes the wording of an advisory.
func termaOwnedKeys(path string, env map[string]string) map[string]bool {
	j, err := harness.LoadJournal(exporter{}.Name(), path)
	if err != nil || j == nil {
		return nil
	}
	owned := map[string]bool{}
	for key, installed := range j.Installed {
		if env[key] == installed {
			owned[key] = true
		}
	}
	return owned
}

// relevantToExporter drops keys for signals Terma is not exporting: they cannot
// redirect telemetry that is never sent.
func relevantToExporter(key string, e harness.Exporter) bool {
	for _, o := range perSignalOverrides {
		if key == o.endpoint || key == o.headers || key == o.protocol {
			return e.HasSignal(o.signal)
		}
	}
	if key == claudeBetaTracingEndpoint {
		return e.HasSignal(harness.SignalTraces) || e.HasSignal(harness.SignalLogs)
	}
	return true
}

// expectedValue is what Terma would itself write for a key, so a value that already
// agrees is not reported as a conflict.
func expectedValue(key string, e harness.Exporter) (string, bool) {
	switch key {
	case harness.EnvOTLPEndpoint:
		return e.Endpoint, true
	}
	for _, o := range perSignalOverrides {
		switch key {
		case o.endpoint:
			return e.SignalEndpoint(o.signal), true
		case o.protocol:
			return harness.ProtocolHTTPProtobuf, true
		}
	}
	return "", false
}

// isRedirect reports whether a key can move telemetry — and Terma's Authorization
// header with it — to a host Terma does not control.
func isRedirect(key string) bool {
	if key == claudeBetaTracingEndpoint {
		return true
	}
	for _, o := range perSignalOverrides {
		if key == o.endpoint {
			return true
		}
	}
	return false
}

// redactIfHeader keeps a header bag out of terminal output: it may hold somebody else's
// credential, and naming the variable is enough to act on.
func redactIfHeader(key, value string) string {
	if key == harness.EnvOTLPHeaders {
		return ""
	}
	for _, o := range perSignalOverrides {
		if key == o.headers {
			return ""
		}
	}
	return value
}
