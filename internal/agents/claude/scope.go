package claude

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/miradorlabs/terma-cli/internal/harness"
)

// Claude Code applies managed settings, `--settings`, `.claude/settings.local.json` and
// `.claude/settings.json` over the user file terma writes, and a shell export may win too, so a
// redirect there gets terma's key; those are conflicts --force cannot clear. The scan covers only
// the directory the CLI runs from.

// claudeLayer is the file a value writes, seen from conflict detection: its scope label, its
// name in prose, and the project files that outrank it.
type claudeLayer struct {
	scope      string
	over       string
	outranking []string
}

// scopeRepositorySettings labels a conflict in the repository's own .claude/settings.json,
// which is clearable because terma is writing that file.
const scopeRepositorySettings = "repository settings"

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

// projectFilesAbove lists both project settings files at each level up to the repository root,
// local before shared as Claude Code applies them.
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

func redirectKeys() []string {
	keys := []string{harness.EnvOTLPEndpoint, harness.EnvOTLPHeaders, claudeBetaTracingEndpoint}
	for _, o := range perSignalOverrides {
		keys = append(keys, o.endpoint, o.headers, o.protocol)
	}
	return keys
}

// environmentConflicts reports redirect settings exported in the shell. Whether a settings `env`
// entry beats a shell variable is undocumented, so any disagreeing export is reported.
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
		// Claude Code reports its own parse error.
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

// captureKeys cannot redirect, but an outranking "off" silently empties every content view while
// status reports connected, so they are scanned there too.
var captureKeys = []string{
	otelLogUserPrompts,
	otelLogAssistantResponse,
	otelLogToolDetails,
	otelLogToolContent,
}

// captureConflictsIn reports content capture an outranking scope turns off: terma always
// asks for it, since the team's policy decides what leaves. Advisory: it never blocks a
// connect, it only says why a dashboard stays empty.
func captureConflictsIn(dir string, l claudeLayer) []harness.Conflict {
	var out []harness.Conflict
	for _, key := range captureKeys {
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
	out = append(out, captureConflictsInProjectFiles(dir, l)...)
	return out
}

// captureConflictsInProjectFiles is captureConflictsIn for project files; a value an
// earlier terma's local connect wrote, by its records under dir, is named as such, with the
// install that removes it.
func captureConflictsInProjectFiles(dir string, l claudeLayer) []harness.Conflict {
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
		owned := termaOwnedKeys(dir, path, doc.Env)
		for _, key := range captureKeys {
			value, ok := doc.Env[key]
			if !ok || value == "" || isOn(value) {
				continue
			}
			reason := "set off in " + path + ", which Claude Code applies over " + l.over
			if owned[key] {
				reason = "turned off in " + path + " by an earlier terma (remove it from that file)"
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

// termaOwnedKeys reports which env keys still hold what this machine's journal under dir
// says terma installed; a colleague's committed layer reads as unowned.
func termaOwnedKeys(dir, path string, env map[string]string) map[string]bool {
	j, err := harness.LoadJournal(dir, exporter{}.Name(), path)
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

// relevantToExporter drops keys for signals terma is not exporting.
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

// redactIfHeader keeps a header bag, which may hold someone's credential, out of output.
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
